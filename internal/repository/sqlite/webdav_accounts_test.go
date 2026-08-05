package sqlite

import (
	"context"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/webdavspace"
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

	// Upsert updates the hash.
	if err := repo.SaveWebDAVAccount(ctx, "editor", "$2a$10$zzzzzzzzzzzzzzzzzzzzzz"); err != nil {
		t.Fatal(err)
	}
	acct, _, _ = repo.GetWebDAVAccount(ctx, "editor")
	if acct.PasswordHash != "$2a$10$zzzzzzzzzzzzzzzzzzzzzz" {
		t.Fatalf("upsert did not update hash: %+v", acct)
	}

	if err := repo.DeleteWebDAVAccount(ctx, "editor"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := repo.GetWebDAVAccount(ctx, "editor"); ok {
		t.Fatal("account still present after delete")
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
