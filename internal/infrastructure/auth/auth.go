package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain"
)

// AuthorizedClient represents a paired client
type AuthorizedClient struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Token     string    `json:"token"`
	CreatedAt time.Time `json:"created_at"`
}

// Store manages authentication credentials and pairing PINs
type Store struct {
	mu           sync.RWMutex
	storePath    string
	activePIN    string
	pinExpiresAt time.Time
	clients      map[string]AuthorizedClient
}

// NewAuthStore initializes auth manager.
// PIN generation is deferred until explicitly requested via GeneratePIN or GetActivePIN.
func NewAuthStore(configDir string) (domain.AuthManager, error) {
	if configDir == "" {
		home, _ := os.UserHomeDir()
		configDir = filepath.Join(home, ".kizuna")
	}
	_ = os.MkdirAll(configDir, 0700)

	store := &Store{
		storePath: filepath.Join(configDir, "authorized_clients.json"),
		clients:   make(map[string]AuthorizedClient),
	}

	_ = store.load()
	return store, nil
}

// GeneratePIN creates a new 6-digit numeric pairing PIN valid for 10 minutes
func (s *Store) GeneratePIN() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	n, _ := rand.Int(rand.Reader, big.NewInt(900000))
	s.activePIN = fmt.Sprintf("%06d", n.Int64()+100000)
	s.pinExpiresAt = time.Now().Add(10 * time.Minute)
	return s.activePIN
}

// GetActivePIN returns active PIN if valid
func (s *Store) GetActivePIN() (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if time.Now().After(s.pinExpiresAt) || s.activePIN == "" {
		return "", false
	}
	return s.activePIN, true
}

// VerifyPIN checks PIN and issues a persistent bearer token
func (s *Store) VerifyPIN(pin, clientName string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if time.Now().After(s.pinExpiresAt) || s.activePIN == "" || s.activePIN != pin {
		return "", domain.ErrInvalidPIN
	}

	// Invalidate PIN after successful pairing
	s.activePIN = ""

	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", err
	}
	token := "kzn_" + hex.EncodeToString(tokenBytes)

	client := AuthorizedClient{
		ID:        fmt.Sprintf("cli_%d", time.Now().UnixNano()),
		Name:      clientName,
		Token:     token,
		CreatedAt: time.Now(),
	}

	s.clients[token] = client
	_ = s.saveLocked()

	return token, nil
}

// ValidateToken checks if a token is authorized.
// "kzn_local" is only valid when the request originates from loopback (127.0.0.1 / ::1).
func (s *Store) ValidateToken(token string) bool {
	if token == "kzn_local" {
		// kzn_local is accepted only via ValidateTokenFromAddr; reject here.
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	_, exists := s.clients[token]
	return exists
}

// ValidateTokenFromAddr checks if a token is authorized, allowing kzn_local only from loopback addresses.
func (s *Store) ValidateTokenFromAddr(token, remoteAddr string) bool {
	if token == "kzn_local" {
		return isLoopbackAddr(remoteAddr)
	}
	return s.ValidateToken(token)
}

// isLoopbackAddr checks if remoteAddr (host:port or host) is a loopback address.
func isLoopbackAddr(remoteAddr string) bool {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// RevokeClient revokes all tokens belonging to a client by ID or Name
func (s *Store) RevokeClient(nameOrID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var tokensToDelete []string
	for token, c := range s.clients {
		if c.Name == nameOrID || c.ID == nameOrID {
			tokensToDelete = append(tokensToDelete, token)
		}
	}

	if len(tokensToDelete) == 0 {
		return fmt.Errorf("no authorized client found matching '%s'", nameOrID)
	}

	for _, token := range tokensToDelete {
		delete(s.clients, token)
	}

	return s.saveLocked()
}

// RevokeToken revokes a specific token
func (s *Store) RevokeToken(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.clients[token]; !exists {
		return fmt.Errorf("token not found")
	}

	delete(s.clients, token)
	return s.saveLocked()
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.storePath)
	if err != nil {
		return err
	}
	var list []AuthorizedClient
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	for _, c := range list {
		s.clients[c.Token] = c
	}
	return nil
}

func (s *Store) saveLocked() error {
	var list []AuthorizedClient
	for _, c := range s.clients {
		list = append(list, c)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.storePath, data, 0600)
}
