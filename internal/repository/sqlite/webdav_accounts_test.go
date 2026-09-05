package sqlite

import (
	"context"
	"slices"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/webdavspace"
)

func openWebDAVRepo(t *testing.T, name string) *Repository {
	t.Helper()
	repo, err := Open(t.TempDir() + "/" + name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestWebDAVAccountLifecycle(t *testing.T) {
	repo := openWebDAVRepo(t, "webdav-accounts.db")
	ctx := context.Background()

	if _, ok, err := repo.GetWebDAVAccount(ctx, "editor"); err != nil || ok {
		t.Fatalf("fresh repo: GetWebDAVAccount = %t, %v; want missing", ok, err)
	}

	if err := repo.SaveWebDAVAccount(ctx, "editor", "$2a$10$abcdefghijklmnopqrstuv"); err != nil {
		t.Fatal(err)
	}
	acct, ok, err := repo.GetWebDAVAccount(ctx, "editor")
	if err != nil || !ok {
		t.Fatalf("GetWebDAVAccount = %t, %v", ok, err)
	}
	if acct.Username != "editor" || acct.PasswordHash != "$2a$10$abcdefghijklmnopqrstuv" {
		t.Fatalf("stored account = %+v", acct)
	}

	// Duplicate Save must return an error, not silently overwrite.
	if err := repo.SaveWebDAVAccount(ctx, "editor", "$2a$10$zzzzzzzzzzzzzzzzzzzzzz"); err == nil {
		t.Fatal("expected duplicate Save to return an error, got nil")
	} else if err.Error() != "account already exists" {
		t.Fatalf("expected 'account already exists', got: %v", err)
	}
	// Original hash must be unchanged after rejected duplicate.
	acct, _, _ = repo.GetWebDAVAccount(ctx, "editor")
	if acct.PasswordHash != "$2a$10$abcdefghijklmnopqrstuv" {
		t.Fatalf("hash changed after rejected duplicate Save: %+v", acct)
	}

	// To rotate a password: delete then re-create.
	if err := repo.DeleteWebDAVAccount(ctx, "editor"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveWebDAVAccount(ctx, "editor", "$2a$10$newnewnewnewnewnewnewnew"); err != nil {
		t.Fatal(err)
	}
	acct, _, _ = repo.GetWebDAVAccount(ctx, "editor")
	if acct.PasswordHash != "$2a$10$newnewnewnewnewnewnewnew" {
		t.Fatalf("password rotation did not store new hash: %+v", acct)
	}

	if err := repo.DeleteWebDAVAccount(ctx, "editor"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := repo.GetWebDAVAccount(ctx, "editor"); ok {
		t.Fatal("account still present after delete")
	}
}

func TestListWebDAVAccountsSorting(t *testing.T) {
	repo := openWebDAVRepo(t, "webdav-sort.db")
	ctx := context.Background()

	// Insert in non-sorted order.
	for _, u := range []string{"charlie", "alice", "bob"} {
		if err := repo.SaveWebDAVAccount(ctx, u, "hash-"+u); err != nil {
			t.Fatal(err)
		}
	}

	accounts, err := repo.ListWebDAVAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alice", "bob", "charlie"}
	if !slices.Equal(accounts, want) {
		t.Fatalf("ListWebDAVAccounts = %v, want sorted %v", accounts, want)
	}
}

func TestWebDAVAccountStoreAdapter(t *testing.T) {
	repo := openWebDAVRepo(t, "webdav-store.db")
	ctx := context.Background()
	store := WebDAVAccountStore{Repo: repo}

	if err := repo.SaveWebDAVAccount(ctx, "editor", "hash"); err != nil {
		t.Fatal(err)
	}
	acct, ok, err := store.GetAccount(ctx, "editor")
	if err != nil || !ok {
		t.Fatalf("adapter GetAccount = %t, %v", ok, err)
	}
	if acct.PasswordHash != "hash" {
		t.Fatalf("adapter hash = %q", acct.PasswordHash)
	}
	var _ webdavspace.AccountStore = store // compile-time check
}
