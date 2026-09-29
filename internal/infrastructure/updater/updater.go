package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const (
	defaultGitHubRepo = "osuki-dev/kizuna"
	cacheTTL          = 24 * time.Hour
)

// ReleaseAsset represents an attached file in GitHub Release
type ReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// Release represents a GitHub Release object
type Release struct {
	TagName     string         `json:"tag_name"`
	Name        string         `json:"name"`
	HTMLURL     string         `json:"html_url"`
	PublishedAt time.Time      `json:"published_at"`
	Assets      []ReleaseAsset `json:"assets"`
}

// UpdateCache stores the last check timestamp and latest version
type UpdateCache struct {
	LastChecked   time.Time `json:"last_checked"`
	LatestVersion string    `json:"latest_version"`
	DownloadURL   string    `json:"download_url"`
}

// Manager manages CLI self-updating
type Manager struct {
	repo       string
	httpClient *http.Client
	cacheDir   string
}

// NewManager initializes a new update manager
func NewManager(repo, cacheDir string) *Manager {
	if repo == "" {
		repo = defaultGitHubRepo
	}
	if cacheDir == "" {
		home, _ := os.UserHomeDir()
		cacheDir = filepath.Join(home, ".kizuna")
	}
	_ = os.MkdirAll(cacheDir, 0755)

	return &Manager{
		repo: repo,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		cacheDir: cacheDir,
	}
}

// FetchLatestRelease queries GitHub API for the latest release
func (m *Manager) FetchLatestRelease(ctx context.Context) (*Release, error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", m.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "kizuna-cli-updater")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to check for updates: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("no releases found for repository %s", m.repo)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github api returned status %d", resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("failed to parse release response: %w", err)
	}

	return &rel, nil
}

// IsNewerVersion returns true if remoteVersion is newer than currentVersion
func IsNewerVersion(currentVersion, remoteVersion string) bool {
	cleanCur := strings.TrimPrefix(currentVersion, "v")
	cleanRem := strings.TrimPrefix(remoteVersion, "v")

	if cleanCur == "dev" || cleanCur == "none" || cleanCur == "" {
		return false
	}
	if cleanRem == "" || cleanRem == "dev" || cleanRem == "none" {
		return false
	}

	vCur := "v" + cleanCur
	vRem := "v" + cleanRem
	if semver.IsValid(vCur) && semver.IsValid(vRem) {
		return semver.Compare(vRem, vCur) > 0
	}

	return cleanCur != cleanRem
}

// Upgrade performs self-update to the latest version
func (m *Manager) Upgrade(ctx context.Context, currentVersion string, out io.Writer) error {
	_, _ = fmt.Fprintln(out, "🔍 Checking for latest release...")

	rel, err := m.FetchLatestRelease(ctx)
	if err != nil {
		return err
	}

	if !IsNewerVersion(currentVersion, rel.TagName) && currentVersion != "dev" {
		_, _ = fmt.Fprintf(out, "✓ kizuna is already up to date (%s)\n", currentVersion)
		return nil
	}

	_, _ = fmt.Fprintf(out, "Found new version: %s (current: %s)\n", rel.TagName, currentVersion)

	// 1. Locate asset matching current OS and Architecture and locate checksums.txt
	var targetAsset *ReleaseAsset
	var checksumAsset *ReleaseAsset

	for _, asset := range rel.Assets {
		name := strings.ToLower(asset.Name)
		if strings.Contains(name, "checksum") || strings.Contains(name, "sha256") {
			checksumAsset = &asset
		}
		osMatch := strings.Contains(name, runtime.GOOS)
		archMatch := strings.Contains(name, runtime.GOARCH)
		if osMatch && archMatch && (strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".zip") || strings.HasSuffix(name, ".tgz")) {
			targetAsset = &asset
		}
	}

	if targetAsset == nil {
		return fmt.Errorf("no binary release found for %s/%s in release %s", runtime.GOOS, runtime.GOARCH, rel.TagName)
	}

	_, _ = fmt.Fprintf(out, "⬇️  Downloading %s (%.1f MB)...\n", targetAsset.Name, float64(targetAsset.Size)/(1024*1024))

	// 2. Download archive
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetAsset.BrowserDownloadURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "kizuna-cli-updater")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	archiveBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read download: %w", err)
	}

	// 3. Cryptographic hash verification if checksums file exists
	if checksumAsset != nil {
		_, _ = fmt.Fprintln(out, "🔐 Verifying SHA256 checksum against "+checksumAsset.Name+"...")
		reqCheck, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumAsset.BrowserDownloadURL, nil)
		if err == nil {
			reqCheck.Header.Set("User-Agent", "kizuna-cli-updater")
			if respCheck, err := m.httpClient.Do(reqCheck); err == nil && respCheck.StatusCode == http.StatusOK {
				defer func() { _ = respCheck.Body.Close() }()
				if checkBytes, err := io.ReadAll(respCheck.Body); err == nil {
					if err := VerifyChecksum(archiveBytes, targetAsset.Name, string(checkBytes)); err != nil {
						return fmt.Errorf("hash verification failed: %w", err)
					}
					_, _ = fmt.Fprintln(out, "✓ SHA256 checksum verified successfully")
				}
			}
		}
	}

	// 4. Extract executable binary
	binaryName := "kizuna"
	if runtime.GOOS == "windows" {
		binaryName = "kizuna.exe"
	}

	binaryData, err := extractBinary(archiveBytes, targetAsset.Name, binaryName)
	if err != nil {
		return fmt.Errorf("failed to extract %s from archive: %w", binaryName, err)
	}

	// 4. Atomically replace currently running executable
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("unable to determine current executable path: %w", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("unable to resolve symlinks for %s: %w", execPath, err)
	}

	if err := applyBinaryUpdate(execPath, binaryData); err != nil {
		return fmt.Errorf("failed to replace executable: %w", err)
	}

	_, _ = fmt.Fprintf(out, "✓ Successfully upgraded kizuna to %s!\n", rel.TagName)
	return nil
}

func extractBinary(archiveData []byte, archiveName, binaryName string) ([]byte, error) {
	if strings.HasSuffix(archiveName, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(archiveData), int64(len(archiveData)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) == binaryName {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer func() { _ = rc.Close() }()
				return io.ReadAll(rc)
			}
		}
	} else {
		// tar.gz
		gr, err := gzip.NewReader(bytes.NewReader(archiveData))
		if err != nil {
			return nil, err
		}
		defer func() { _ = gr.Close() }()

		tr := tar.NewReader(gr)
		for {
			header, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			if filepath.Base(header.Name) == binaryName {
				return io.ReadAll(tr)
			}
		}
	}

	return nil, fmt.Errorf("binary '%s' not found inside archive", binaryName)
}

func applyBinaryUpdate(targetPath string, newBinary []byte) error {
	dir := filepath.Dir(targetPath)
	tmpPath := filepath.Join(dir, fmt.Sprintf(".kizuna_update_%d", time.Now().UnixNano()))

	// Write new binary with executable permissions
	if err := os.WriteFile(tmpPath, newBinary, 0755); err != nil {
		return err
	}

	if runtime.GOOS == "windows" {
		// Windows: rename current executable to .old first
		oldPath := targetPath + ".old"
		_ = os.Remove(oldPath)
		if err := os.Rename(targetPath, oldPath); err != nil {
			_ = os.Remove(tmpPath)
			return err
		}
		if err := os.Rename(tmpPath, targetPath); err != nil {
			_ = os.Rename(oldPath, targetPath)
			return err
		}
	} else {
		// Unix / macOS: atomic rename replaces inode directly
		if err := os.Rename(tmpPath, targetPath); err != nil {
			_ = os.Remove(tmpPath)
			return err
		}
	}

	return nil
}

// CheckUpdateNotice checks the cache or queries GitHub if cache expired
func (m *Manager) CheckUpdateNotice(currentVersion string) string {
	if currentVersion == "dev" || currentVersion == "" {
		return ""
	}

	cacheFile := filepath.Join(m.cacheDir, "update_check.json")
	var cache UpdateCache

	data, err := os.ReadFile(cacheFile)
	if err == nil {
		_ = json.Unmarshal(data, &cache)
	}

	now := time.Now()
	if now.Sub(cache.LastChecked) > cacheTTL {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		rel, err := m.FetchLatestRelease(ctx)
		if err == nil && rel != nil {
			cache.LastChecked = now
			cache.LatestVersion = rel.TagName
			cache.DownloadURL = rel.HTMLURL

			cacheData, _ := json.Marshal(cache)
			_ = os.WriteFile(cacheFile, cacheData, 0644)
		}
	}

	if cache.LatestVersion != "" && IsNewerVersion(currentVersion, cache.LatestVersion) {
		return fmt.Sprintf("\n💡 A new version of kizuna is available: %s (current: %s)\n   Run 'kizuna upgrade' to update automatically.\n",
			cache.LatestVersion, currentVersion)
	}

	return ""
}

// VerifyChecksum validates that the SHA256 sum of data matches the recorded hash for assetName in checksums.txt
func VerifyChecksum(data []byte, assetName, checksumsContent string) error {
	sum := sha256.Sum256(data)
	actualHash := hex.EncodeToString(sum[:])

	lines := strings.Split(checksumsContent, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			expectedHash := strings.ToLower(parts[0])
			fileName := filepath.Base(parts[1])
			if fileName == assetName {
				if actualHash != expectedHash {
					return fmt.Errorf("sha256 mismatch for %s: expected %s, got %s", assetName, expectedHash, actualHash)
				}
				return nil
			}
		}
	}
	return fmt.Errorf("hash for %s not found in checksums file", assetName)
}
