package sqlite

import (
	"context"
	"path/filepath"
	"testing"
)

func TestIntegrityCheckHealthy(t *testing.T) {
	repo, err := Open(filepath.Join(t.TempDir(), "integrity.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := repo.IntegrityCheck(context.Background()); err != nil {
		t.Fatalf("IntegrityCheck on a fresh healthy library = %v, want nil", err)
	}
}
