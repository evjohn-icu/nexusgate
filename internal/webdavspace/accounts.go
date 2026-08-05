package webdavspace

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

// ErrInvalidCredentials reports a Basic-Auth attempt whose username or
// password did not match any account.
var ErrInvalidCredentials = errors.New("invalid WebDAV credentials")

// Account is one WebDAV user. Passwords are stored only as bcrypt hashes; the
// plaintext is never persisted.
type Account struct {
	Username string
	// PasswordHash is a bcrypt hash of the account password.
	PasswordHash string
}

// AccountStore is the credential source for WebDAV Basic Auth. The Hub
// provides a repository-backed implementation; the in-memory one here serves
// tests and small deployments.
type AccountStore interface {
	// GetAccount returns the account for username, or (nil, false) when
	// unknown.
	GetAccount(ctx context.Context, username string) (*Account, bool, error)
}

// memAccountStore is an AccountStore kept entirely in memory (tests).
type memAccountStore struct {
	mu       sync.RWMutex
	accounts map[string]*Account
}

// NewMemAccountStore returns an empty in-memory account store.
func NewMemAccountStore() *memAccountStore {
	return &memAccountStore{accounts: map[string]*Account{}}
}

func (m *memAccountStore) GetAccount(_ context.Context, username string) (*Account, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.accounts[username]
	return a, ok, nil
}

// HashPassword returns a bcrypt hash of the plaintext password. It is the
// only place plaintext is turned into a stored credential; the caller passes
// only the hash onward.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// CreateAccount hashes the plaintext password with bcrypt and stores it. It
// returns an error when the username is taken or the password is empty.
func (m *memAccountStore) CreateAccount(username, password string) error {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return errors.New("username and password are required")
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.accounts[username]; exists {
		return errors.New("account already exists")
	}
	m.accounts[username] = &Account{Username: username, PasswordHash: hash}
	return nil
}

// Authenticate checks username/password against the store using a
// constant-time comparison of the bcrypt result. It never leaks whether the
// username or the password was wrong.
func Authenticate(ctx context.Context, store AccountStore, username, password string) error {
	account, ok, err := store.GetAccount(ctx, username)
	if err != nil {
		return err
	}
	// Always run a comparison even when the account is unknown, so timing
	// does not reveal account existence.
	knownHash := ""
	if ok {
		knownHash = account.PasswordHash
	}
	hashBytes := []byte(knownHash)
	candidate := []byte(password)
	if !ok {
		// Compare against a fixed dummy hash to keep timing uniform.
		hashBytes = []byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy")
	}
	if err := bcrypt.CompareHashAndPassword(hashBytes, candidate); err != nil {
		return ErrInvalidCredentials
	}
	if !ok {
		return ErrInvalidCredentials
	}
	return nil
}

// constantTimeEq is a helper for callers that need a boolean without error.
func constantTimeEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
