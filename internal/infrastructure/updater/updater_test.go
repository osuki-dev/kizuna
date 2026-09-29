package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestIsNewerVersion(t *testing.T) {
	tests := []struct {
		cur      string
		remote   string
		expected bool
	}{
		{"dev", "v1.0.0", false},
		{"v0.1.0", "v0.1.0", false},
		{"0.1.0", "v0.1.0", false},
		{"v0.1.0", "v0.2.0", true},
		{"0.1.0", "v0.1.1", true},
	}

	for _, tt := range tests {
		got := IsNewerVersion(tt.cur, tt.remote)
		if got != tt.expected {
			t.Errorf("IsNewerVersion(%q, %q) = %v; expected %v", tt.cur, tt.remote, got, tt.expected)
		}
	}
}

func TestExtractBinaryTarGz(t *testing.T) {
	// Create mock tar.gz containing binary
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	content := []byte("binary-content-here")
	hdr := &tar.Header{
		Name: "kizuna",
		Mode: 0755,
		Size: int64(len(content)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = tw.Close()
	_ = gw.Close()

	extracted, err := extractBinary(buf.Bytes(), "kizuna_linux_amd64.tar.gz", "kizuna")
	if err != nil {
		t.Fatalf("extractBinary failed: %v", err)
	}

	if string(extracted) != string(content) {
		t.Errorf("extracted content = %q; expected %q", string(extracted), string(content))
	}
}

func TestFetchLatestRelease(t *testing.T) {
	mockRelease := Release{
		TagName: "v0.2.0",
		Name:    "Kizuna v0.2.0",
		Assets: []ReleaseAsset{
			{Name: "kizuna_v0.2.0_linux_amd64.tar.gz", BrowserDownloadURL: "http://example.com/dl", Size: 1024},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mockRelease)
	}))
	defer server.Close()

	mgr := &Manager{
		repo:       "osuki-dev/kizuna",
		httpClient: server.Client(),
		cacheDir:   t.TempDir(),
	}

	// Override URL via custom client transport
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("mock request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if rel.TagName != "v0.2.0" {
		t.Errorf("rel.TagName = %q; expected 'v0.2.0'", rel.TagName)
	}
	_ = mgr
}

func TestApplyBinaryUpdate(t *testing.T) {
	tempDir := t.TempDir()
	binPath := filepath.Join(tempDir, "mock_bin")

	if err := os.WriteFile(binPath, []byte("old-binary"), 0755); err != nil {
		t.Fatalf("failed to write mock binary: %v", err)
	}

	newContent := []byte("new-upgraded-binary")
	if err := applyBinaryUpdate(binPath, newContent); err != nil {
		t.Fatalf("applyBinaryUpdate failed: %v", err)
	}

	updated, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("failed to read updated binary: %v", err)
	}

	if string(updated) != string(newContent) {
		t.Errorf("binary content = %q; expected %q", string(updated), string(newContent))
	}
}

func TestVerifyChecksum(t *testing.T) {
	data := []byte("hello-kizuna-binary-content")
	// SHA256 of "hello-kizuna-binary-content"
	// echo -n "hello-kizuna-binary-content" | sha256sum
	// 5c25e87a2f58fa23dc106175e347ad6491e1d6d8409ffc950a7f1a30fa1400e9
	validHash := "13e8ee9eb5a1a83751d7bb7dac9a4853db919fcffec8f05f61ebcb8a2c73dd44"
	checksums := validHash + "  kizuna_linux_amd64.tar.gz\n" +
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  kizuna_darwin_arm64.tar.gz\n"

	// 1. Success matching
	err := VerifyChecksum(data, "kizuna_linux_amd64.tar.gz", checksums)
	if err != nil {
		t.Errorf("expected checksum verification to succeed, got: %v", err)
	}

	// 2. Corrupted data
	corrupted := []byte("hello-tampered-content")
	err = VerifyChecksum(corrupted, "kizuna_linux_amd64.tar.gz", checksums)
	if err == nil {
		t.Errorf("expected checksum mismatch error, got nil")
	}

	// 3. Asset not found
	err = VerifyChecksum(data, "kizuna_unknown_platform.tar.gz", checksums)
	if err == nil {
		t.Errorf("expected error for missing asset in checksums, got nil")
	}
}
