package api_test

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/osuki-dev/kizuna/internal/adapter/api"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/osuki-dev/kizuna/internal/infrastructure/auth"
)

func TestNodeMetaEndpoints(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)

	authStore, err := auth.NewAuthStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create auth store: %v", err)
	}

	pin, _ := authStore.GetActivePIN()
	token, err := authStore.VerifyPIN(pin, "test-client")
	if err != nil {
		t.Fatalf("failed to verify pin: %v", err)
	}

	srv := api.NewServer(authStore, nil, nil, nil, "node_test_1", "test-node")
	handler := srv.Handler()

	// 1. GET initial meta
	reqGet, _ := http.NewRequest(http.MethodGet, "/api/v1/node/meta", nil)
	reqGet.Header.Set("Authorization", "Bearer "+token)
	rrGet := httptest.NewRecorder()
	handler.ServeHTTP(rrGet, reqGet)

	if rrGet.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/node/meta returned status %d", rrGet.Code)
	}

	// 2. POST update meta
	update := entity.NodeMetaUpdate{
		Tags: []string{"home", "dev"},
		Host: "mac-mini.local",
		IP:   "10.0.0.9",
	}
	body, _ := json.Marshal(update)
	reqPost, _ := http.NewRequest(http.MethodPost, "/api/v1/node/meta", bytes.NewReader(body))
	reqPost.Header.Set("Authorization", "Bearer "+token)
	reqPost.Header.Set("Content-Type", "application/json")
	rrPost := httptest.NewRecorder()
	handler.ServeHTTP(rrPost, reqPost)

	if rrPost.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/node/meta returned status %d: %s", rrPost.Code, rrPost.Body.String())
	}

	// 3. Verify GET returns updated values
	reqGet2, _ := http.NewRequest(http.MethodGet, "/api/v1/node/meta", nil)
	reqGet2.Header.Set("Authorization", "Bearer "+token)
	rrGet2 := httptest.NewRecorder()
	handler.ServeHTTP(rrGet2, reqGet2)

	var res map[string]any
	_ = json.Unmarshal(rrGet2.Body.Bytes(), &res)
	if res["host"] != "mac-mini.local" {
		t.Errorf("expected host 'mac-mini.local', got %v", res["host"])
	}
	if res["ip"] != "10.0.0.9" {
		t.Errorf("expected ip '10.0.0.9', got %v", res["ip"])
	}

	// 4. Test Revocation
	reqRevoke, _ := http.NewRequest(http.MethodPost, "/api/v1/auth/revoke", bytes.NewReader([]byte(`{"name_or_id":"test-client"}`)))
	reqRevoke.Header.Set("Authorization", "Bearer "+token)
	rrRevoke := httptest.NewRecorder()
	handler.ServeHTTP(rrRevoke, reqRevoke)

	if rrRevoke.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/auth/revoke returned status %d", rrRevoke.Code)
	}

	// 5. Subsequent request with revoked token should be 401 Unauthorized
	reqUnauthorized, _ := http.NewRequest(http.MethodGet, "/api/v1/node/meta", nil)
	reqUnauthorized.Header.Set("Authorization", "Bearer "+token)
	rrUnauthorized := httptest.NewRecorder()
	handler.ServeHTTP(rrUnauthorized, reqUnauthorized)

	if rrUnauthorized.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401 Unauthorized for revoked token, got %d", rrUnauthorized.Code)
	}
}

func TestServeConnLifecycle(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)

	authStore, err := auth.NewAuthStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create auth store: %v", err)
	}

	srv := api.NewServer(authStore, nil, nil, nil, "node_test_1", "test-node")

	clientConn, serverConn := net.Pipe()

	done := make(chan struct{})
	go func() {
		srv.ServeConn(serverConn)
		close(done)
	}()

	// Send an HTTP request from client side
	reqText := "GET /api/v1/node/members HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"
	go func() {
		_, _ = clientConn.Write([]byte(reqText))
		buf := make([]byte, 1024)
		_, _ = clientConn.Read(buf)
		_ = clientConn.Close()
	}()

	select {
	case <-done:
		// Succeeded: ServeConn cleanly returned upon connection close!
	case <-time.After(2 * time.Second):
		t.Fatal("ServeConn deadlocked or leaked: did not exit after client connection was closed")
	}
}
