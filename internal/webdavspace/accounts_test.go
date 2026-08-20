package webdavspace

import (
	"context"
	"strings"
	"testing"
)

func TestCreateAccountAndAuthenticate(t *testing.T) {
	store := NewMemAccountStore()
	if err := store.CreateAccount("editor", "s3cret"); err != nil {
		t.Fatal(err)
	}
	if err := Authenticate(context.Background(), store, "editor", "s3cret"); err != nil {
		t.Fatalf("valid credentials rejected: %v", err)
	}
	if err := Authenticate(context.Background(), store, "editor", "wrong"); err != ErrInvalidCredentials {
		t.Fatalf("wrong password = %v, want ErrInvalidCredentials", err)
	}
	if err := Authenticate(context.Background(), store, "ghost", "s3cret"); err != ErrInvalidCredentials {
		t.Fatalf("unknown user = %v, want ErrInvalidCredentials", err)
	}
}

func TestCreateAccountRejectsDuplicateAndEmpty(t *testing.T) {
	store := NewMemAccountStore()
	if err := store.CreateAccount("editor", "pw"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAccount("editor", "pw2"); err == nil {
		t.Fatal("duplicate account accepted")
	}
	if err := store.CreateAccount("", "pw"); err == nil {
		t.Fatal("empty username accepted")
	}
	if err := store.CreateAccount("x", ""); err == nil {
		t.Fatal("empty password accepted")
	}
}

func TestPasswordStoredAsHashOnly(t *testing.T) {
	store := NewMemAccountStore()
	if err := store.CreateAccount("editor", "plaintext-secret"); err != nil {
		t.Fatal(err)
	}
	a, ok, err := store.GetAccount(context.Background(), "editor")
	if err != nil || !ok {
		t.Fatalf("GetAccount = %v, %t, %v", a, ok, err)
	}
	if strings.Contains(a.PasswordHash, "plaintext-secret") {
		t.Fatal("plaintext leaked into stored hash")
	}
	if !strings.HasPrefix(a.PasswordHash, "$2") {
		t.Fatalf("hash does not look like bcrypt: %q", a.PasswordHash)
	}
}

// WEBDAV-002: bcrypt truncates input at 72 bytes, so a password longer than
// MaxWebDAVPasswordBytes must be refused up front rather than silently
// hashed into a credential that only its first bytes protect.
func TestHashPasswordRejectsOversizedPassword(t *testing.T) {
	long := strings.Repeat("x", MaxWebDAVPasswordBytes+1)
	if _, err := HashPassword(long); err == nil {
		t.Fatal("password longer than MaxWebDAVPasswordBytes was hashed")
	}
	// Exactly the bound is accepted (and hashes to real bcrypt).
	atBound := strings.Repeat("y", MaxWebDAVPasswordBytes)
	hash, err := HashPassword(atBound)
	if err != nil {
		t.Fatalf("password at MaxWebDAVPasswordBytes rejected: %v", err)
	}
	if !strings.HasPrefix(hash, "$2") {
		t.Fatalf("hash does not look like bcrypt: %q", hash)
	}

	// The in-memory store's CreateAccount goes through HashPassword, so the
	// same bound protects the account path.
	store := NewMemAccountStore()
	if err := store.CreateAccount("editor", long); err == nil {
		t.Fatal("CreateAccount accepted an oversized password")
	}
	if err := store.CreateAccount("editor", atBound); err != nil {
		t.Fatalf("CreateAccount rejected password at the bound: %v", err)
	}
}
