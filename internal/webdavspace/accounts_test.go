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
