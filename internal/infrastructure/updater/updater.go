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
	"net"
	"net/http"
	"os"
	"os/exec"
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

// UpgradeStep represents a stage in the upgrade pipeline
type UpgradeStep string

const (
	StepChecking    UpgradeStep = "checking"
	StepUpToDate    UpgradeStep = "up_to_date"
	StepFound       UpgradeStep = "found"
	StepDownloading UpgradeStep = "downloading"
	StepVerifying   UpgradeStep = "verifying"
	StepExtracting  UpgradeStep = "extracting"
	StepApplying    UpgradeStep = "applying"
	StepComplete    UpgradeStep = "complete"
	StepFailed      UpgradeStep = "failed"
)

// UpgradeEvent conveys progress and state during self-update
type UpgradeEvent struct {
	Step        UpgradeStep
	Message     string
	CurrentVer  string
	LatestVer   string
	Release     *Release
	Asset       *ReleaseAsset
	Downloaded  int64
	TotalSize   int64
	Speed       float64 // bytes/sec
	Percent     float64
	TargetExec  string
	Err         error
}

// Manager manages CLI self-updating
type Manager struct {
	repo       string
	apiBaseURL string
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

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &Manager{
		repo:       repo,
		apiBaseURL: "https://api.github.com",
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   0, // No overall timeout; slow connections (even a few KB/s) can finish downloading without deadline cancellation
		},
		cacheDir: cacheDir,
	}
}

// FetchLatestRelease queries GitHub API for the latest release
func (m *Manager) FetchLatestRelease(ctx context.Context) (*Release, error) {
	// Ensure release metadata check has a sensible deadline if caller didn't specify one
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
	}

	baseURL := m.apiBaseURL
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	apiURL := fmt.Sprintf("%s/repos/%s/releases/latest", baseURL, m.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "kizuna-cli-updater")
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else if token := os.Getenv("GH_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

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

type progressReader struct {
	reader     io.Reader
	total      int64
	current    int64
	lastTime   time.Time
	lastBytes  int64
	onProgress func(current, total int64, speed float64)
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	if n > 0 {
		pr.current += int64(n)
		now := time.Now()
		elapsed := now.Sub(pr.lastTime).Seconds()
		if elapsed >= 0.05 || pr.current >= pr.total || err != nil {
			var speed float64
			if elapsed > 0 {
				speed = float64(pr.current-pr.lastBytes) / elapsed
			}
			pr.lastTime = now
			pr.lastBytes = pr.current
			if pr.onProgress != nil {
				pr.onProgress(pr.current, pr.total, speed)
			}
		}
	}
	return n, err
}

// UpgradeWithProgress performs self-update with detailed progress events
func (m *Manager) UpgradeWithProgress(ctx context.Context, currentVersion string, onEvent func(UpgradeEvent)) error {
	emit := func(ev UpgradeEvent) {
		if onEvent != nil {
			onEvent(ev)
		}
	}

	emit(UpgradeEvent{
		Step:       StepChecking,
		Message:    "Checking for latest release...",
		CurrentVer: currentVersion,
	})

	rel, err := m.FetchLatestRelease(ctx)
	if err != nil {
		emit(UpgradeEvent{
			Step:       StepFailed,
			Message:    err.Error(),
			CurrentVer: currentVersion,
			Err:        err,
		})
		return err
	}

	if !IsNewerVersion(currentVersion, rel.TagName) && currentVersion != "dev" {
		emit(UpgradeEvent{
			Step:       StepUpToDate,
			Message:    fmt.Sprintf("kizuna is already up to date (%s)", currentVersion),
			CurrentVer: currentVersion,
			LatestVer:  rel.TagName,
			Release:    rel,
		})
		return nil
	}

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
		err := fmt.Errorf("no binary release found for %s/%s in release %s", runtime.GOOS, runtime.GOARCH, rel.TagName)
		emit(UpgradeEvent{
			Step:       StepFailed,
			Message:    err.Error(),
			CurrentVer: currentVersion,
			LatestVer:  rel.TagName,
			Release:    rel,
			Err:        err,
		})
		return err
	}

	emit(UpgradeEvent{
		Step:       StepFound,
		Message:    fmt.Sprintf("Found update %s (current: %s)", rel.TagName, currentVersion),
		CurrentVer: currentVersion,
		LatestVer:  rel.TagName,
		Release:    rel,
		Asset:      targetAsset,
		TotalSize:  targetAsset.Size,
	})

	// 2. Download archive with live streaming progress
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetAsset.BrowserDownloadURL, nil)
	if err != nil {
		emit(UpgradeEvent{
			Step:    StepFailed,
			Message: err.Error(),
			Err:     err,
		})
		return err
	}
	req.Header.Set("User-Agent", "kizuna-cli-updater")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		err = fmt.Errorf("download failed: %w", err)
		emit(UpgradeEvent{
			Step:    StepFailed,
			Message: err.Error(),
			Err:     err,
		})
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	contentLength := resp.ContentLength
	if contentLength <= 0 {
		contentLength = targetAsset.Size
	}

	pr := &progressReader{
		reader:   resp.Body,
		total:    contentLength,
		lastTime: time.Now(),
		onProgress: func(current, total int64, speed float64) {
			var pct float64
			if total > 0 {
				pct = float64(current) / float64(total) * 100
			}
			emit(UpgradeEvent{
				Step:       StepDownloading,
				Message:    fmt.Sprintf("Downloading %s...", targetAsset.Name),
				CurrentVer: currentVersion,
				LatestVer:  rel.TagName,
				Release:    rel,
				Asset:      targetAsset,
				Downloaded: current,
				TotalSize:  total,
				Speed:      speed,
				Percent:    pct,
			})
		},
	}

	archiveBytes, err := io.ReadAll(pr)
	if err != nil {
		err = fmt.Errorf("failed to read download: %w", err)
		emit(UpgradeEvent{
			Step:    StepFailed,
			Message: err.Error(),
			Err:     err,
		})
		return err
	}

	// 3. Cryptographic hash verification if checksums file exists
	if checksumAsset != nil {
		emit(UpgradeEvent{
			Step:       StepVerifying,
			Message:    "Verifying cryptographic SHA256 checksum...",
			CurrentVer: currentVersion,
			LatestVer:  rel.TagName,
			Release:    rel,
			Asset:      targetAsset,
		})
		reqCheck, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumAsset.BrowserDownloadURL, nil)
		if err == nil {
			reqCheck.Header.Set("User-Agent", "kizuna-cli-updater")
			if respCheck, err := m.httpClient.Do(reqCheck); err == nil && respCheck.StatusCode == http.StatusOK {
				defer func() { _ = respCheck.Body.Close() }()
				if checkBytes, err := io.ReadAll(respCheck.Body); err == nil {
					if err := VerifyChecksum(archiveBytes, targetAsset.Name, string(checkBytes)); err != nil {
						err = fmt.Errorf("hash verification failed: %w", err)
						emit(UpgradeEvent{
							Step:    StepFailed,
							Message: err.Error(),
							Err:     err,
						})
						return err
					}
				}
			}
		}
	}

	// 4. Extract executable binary
	emit(UpgradeEvent{
		Step:       StepExtracting,
		Message:    "Extracting binary archive...",
		CurrentVer: currentVersion,
		LatestVer:  rel.TagName,
		Release:    rel,
		Asset:      targetAsset,
	})

	binaryName := "kizuna"
	if runtime.GOOS == "windows" {
		binaryName = "kizuna.exe"
	}

	binaryData, err := extractBinary(archiveBytes, targetAsset.Name, binaryName)
	if err != nil {
		err = fmt.Errorf("failed to extract %s from archive: %w", binaryName, err)
		emit(UpgradeEvent{
			Step:    StepFailed,
			Message: err.Error(),
			Err:     err,
		})
		return err
	}

	// 5. Atomically replace currently running executable
	emit(UpgradeEvent{
		Step:       StepApplying,
		Message:    "Atomically replacing executable...",
		CurrentVer: currentVersion,
		LatestVer:  rel.TagName,
		Release:    rel,
		Asset:      targetAsset,
	})

	execPath, err := os.Executable()
	if err != nil {
		err = fmt.Errorf("unable to determine current executable path: %w", err)
		emit(UpgradeEvent{
			Step:    StepFailed,
			Message: err.Error(),
			Err:     err,
		})
		return err
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		err = fmt.Errorf("unable to resolve symlinks for %s: %w", execPath, err)
		emit(UpgradeEvent{
			Step:    StepFailed,
			Message: err.Error(),
			Err:     err,
		})
		return err
	}

	if err := applyBinaryUpdate(execPath, binaryData); err != nil {
		err = fmt.Errorf("failed to replace executable: %w", err)
		emit(UpgradeEvent{
			Step:    StepFailed,
			Message: err.Error(),
			Err:     err,
		})
		return err
	}

	emit(UpgradeEvent{
		Step:       StepComplete,
		Message:    fmt.Sprintf("Successfully upgraded to %s!", rel.TagName),
		CurrentVer: currentVersion,
		LatestVer:  rel.TagName,
		Release:    rel,
		Asset:      targetAsset,
		TargetExec: execPath,
	})
	return nil
}

// Upgrade performs self-update printing clean text lines to out
func (m *Manager) Upgrade(ctx context.Context, currentVersion string, out io.Writer) error {
	return m.UpgradeWithProgress(ctx, currentVersion, func(ev UpgradeEvent) {
		switch ev.Step {
		case StepChecking:
			_, _ = fmt.Fprintln(out, "🔍 Checking for latest release...")
		case StepUpToDate:
			_, _ = fmt.Fprintf(out, "✓ kizuna is already up to date (%s)\n", ev.CurrentVer)
		case StepFound:
			_, _ = fmt.Fprintf(out, "Found new version: %s (current: %s)\n", ev.LatestVer, ev.CurrentVer)
			if ev.Asset != nil {
				_, _ = fmt.Fprintf(out, "⬇️  Downloading %s (%.1f MB)...\n", ev.Asset.Name, float64(ev.Asset.Size)/(1024*1024))
			}
		case StepVerifying:
			_, _ = fmt.Fprintln(out, "🔐 Verifying SHA256 checksum...")
		case StepExtracting:
			_, _ = fmt.Fprintln(out, "⚡ Extracting binary archive...")
		case StepApplying:
			_, _ = fmt.Fprintln(out, "🔄 Applying binary update...")
		case StepComplete:
			_, _ = fmt.Fprintf(out, "✓ Successfully upgraded kizuna to %s!\n", ev.LatestVer)
		case StepFailed:
			if ev.Err != nil {
				_, _ = fmt.Fprintf(out, "✖ Upgrade failed: %v\n", ev.Err)
			}
		}
	})
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

	// On macOS (darwin), re-apply ad-hoc codesign and clear quarantine to avoid AMFI SIGKILL
	if runtime.GOOS == "darwin" {
		_ = exec.Command("xattr", "-cr", targetPath).Run()
		_ = exec.Command("codesign", "--force", "--deep", "-s", "-", targetPath).Run()
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
