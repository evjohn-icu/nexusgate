package hubauth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureAdminTokenPersistsMode0600AndReusesToken(t *testing.T) {
	dir := t.TempDir()
	first, err := EnsureAdminToken(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnsureAdminToken(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || first != second {
		t.Fatalf("tokens first=%q second=%q", first, second)
	}
	info, err := os.Stat(filepath.Join(dir, "admin-token"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("admin token mode=%#o want 0600", info.Mode().Perm())
	}
}
