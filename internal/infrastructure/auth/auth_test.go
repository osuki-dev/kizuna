package auth_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/osuki-dev/kizuna/internal/infrastructure/auth"
)

func TestAuthStoreLifecycleAndRevocation(t *testing.T) {
	tempDir := t.TempDir()
	store, err := auth.NewAuthStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create auth store: %v", err)
	}

	// PIN is not generated eagerly; must call GeneratePIN first
	store.GeneratePIN()
	pin, ok := store.GetActivePIN()
	if !ok || len(pin) != 6 {
		t.Fatalf("expected valid 6-digit PIN, got %s (ok=%v)", pin, ok)
	}

	// Verify PIN
	token, err := store.VerifyPIN(pin, "laptop-ryu")
	if err != nil {
		t.Fatalf("failed to verify pin: %v", err)
	}
	if !store.ValidateToken(token) {
		t.Fatalf("expected token to be valid")
	}

	// Verify PIN is consumed
	_, ok = store.GetActivePIN()
	if ok {
		t.Fatalf("expected PIN to be consumed after successful verification")
	}

	// Revoke client
	if err := store.RevokeClient("laptop-ryu"); err != nil {
		t.Fatalf("failed to revoke client: %v", err)
	}

	if store.ValidateToken(token) {
		t.Fatalf("expected token to be invalid after revocation")
	}
}

func TestPairingPersistenceFailureDoesNotIssueToken(t *testing.T) {
	dir := t.TempDir()
	store, err := auth.NewAuthStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	pin := store.GeneratePIN()
	// Replacing the destination file with a directory makes the atomic rename fail.
	if err := os.Mkdir(filepath.Join(dir, "authorized_clients.json"), 0700); err != nil {
		t.Fatal(err)
	}
	token, err := store.VerifyPIN(pin, "client")
	if err == nil || token != "" {
		t.Fatal("pairing returned a credential that was not persisted")
	}
	if _, valid := store.GetActivePIN(); !valid {
		t.Fatal("failed pairing consumed the PIN")
	}
}

func TestAuthStoreRejectsCorruptState(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "authorized_clients.json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.NewAuthStore(dir); err == nil {
		t.Fatal("corrupt credentials were silently ignored")
	}
}

func TestFailedRevocationKeepsCredentialActive(t *testing.T) {
	for _, byToken := range []bool{false, true} {
		t.Run(fmt.Sprintf("by_token_%t", byToken), func(t *testing.T) {
			dir := t.TempDir()
			store, err := auth.NewAuthStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			pin := store.GeneratePIN()
			token, err := store.VerifyPIN(pin, "client")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "authorized_clients.json")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			if byToken {
				err = store.RevokeToken(token)
			} else {
				err = store.RevokeClient("client")
			}
			if err == nil {
				t.Fatal("revocation concealed persistence failure")
			}
			if !store.ValidateToken(token) {
				t.Fatal("failed revocation left memory inconsistent with persisted state")
			}
		})
	}
}
