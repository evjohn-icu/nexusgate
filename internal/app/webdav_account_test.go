package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/media"
	sqlite "github.com/evjohn-icu/timingdex/internal/repository/sqlite"
	"github.com/evjohn-icu/timingdex/internal/webdavspace"
)

// newWebDAVAccountTestService builds a Service whose WebDAV account store is
// repository-backed (as in production, setupWebDAVDelivery in cmd/timingdex),
// because CreateWebDAVAccount deliberately refuses a non-repository store.
func newWebDAVAccountTestService(t *testing.T) *Service {
	t.Helper()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "webdav-accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	store := sqlite.WebDAVAccountStore{Repo: repo}
	service, err := NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	service.SetWebDAVSpaceManager(nil, store)
	return service
}

// WEBDAV-002: bcrypt truncates input at 72 bytes, so the service must refuse a
// password longer than webdavspace.MaxWebDAVPasswordBytes with
// ErrWebDAVAccountInvalid (mapped to 400 by the API) before any hashing, while
// a password exactly at the bound is accepted.
func TestCreateWebDAVAccountRejectsOversizedPassword(t *testing.T) {
	service := newWebDAVAccountTestService(t)
	ctx := context.Background()

	long := strings.Repeat("x", webdavspace.MaxWebDAVPasswordBytes+1)
	if err := service.CreateWebDAVAccount(ctx, "editor", long); !errors.Is(err, ErrWebDAVAccountInvalid) {
		t.Fatalf("CreateWebDAVAccount(oversized) = %v, want ErrWebDAVAccountInvalid", err)
	}
	// The oversized password must not have been stored.
	if _, exists, err := service.webdavAccounts.GetAccount(ctx, "editor"); err != nil {
		t.Fatal(err)
	} else if exists {
		t.Fatal("oversized password created an account")
	}

	atBound := strings.Repeat("y", webdavspace.MaxWebDAVPasswordBytes)
	if err := service.CreateWebDAVAccount(ctx, "editor", atBound); err != nil {
		t.Fatalf("CreateWebDAVAccount(at bound) = %v, want nil", err)
	}

	// The pre-existing invalid-shape refusals still hold.
	if err := service.CreateWebDAVAccount(ctx, "", "pw"); !errors.Is(err, ErrWebDAVAccountInvalid) {
		t.Fatalf("empty username = %v, want ErrWebDAVAccountInvalid", err)
	}
	if err := service.CreateWebDAVAccount(ctx, "other", ""); !errors.Is(err, ErrWebDAVAccountInvalid) {
		t.Fatalf("empty password = %v, want ErrWebDAVAccountInvalid", err)
	}
}
