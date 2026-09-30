package auth_test

import (
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
