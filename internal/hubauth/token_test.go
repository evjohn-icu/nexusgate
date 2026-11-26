package hubauth

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestEnsureAdminTokenPersistsPrivatelyAndReusesToken(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := EnsureAdminToken(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnsureAdminToken(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || first != second {
		t.Fatal("administrator token was empty or changed")
	}
	path := filepath.Join(dir, "admin-token")
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateTokenFile(path, info); err != nil {
		t.Fatal(err)
	}
}

func secureHubDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestEnsureTokenRejectsUnsafeFilesAndDirectories(t *testing.T) {
	t.Run("symlink token", func(t *testing.T) {
		dir := secureHubDir(t)
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, []byte("secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, "admin-token")); err != nil {
			t.Fatal(err)
		}
		if _, err := EnsureAdminToken(dir, ""); err == nil {
			t.Fatal("expected symlink rejection")
		}
	})
	t.Run("directory token", func(t *testing.T) {
		dir := secureHubDir(t)
		if err := os.Mkdir(filepath.Join(dir, "admin-token"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := EnsureAdminToken(dir, ""); err == nil {
			t.Fatal("expected non-regular rejection")
		}
	})
	for _, mode := range []os.FileMode{0o644, 0o400, 0o660} {
		if runtime.GOOS == "windows" {
			break
		}
		t.Run(fmt.Sprintf("insecure token %o", mode), func(t *testing.T) {
			dir := secureHubDir(t)
			path := filepath.Join(dir, "admin-token")
			if err := os.WriteFile(path, []byte("secret\n"), mode); err != nil {
				t.Fatal(err)
			}
			if _, err := EnsureAdminToken(dir, ""); err == nil {
				t.Fatal("expected permission rejection")
			}
		})
	}
	t.Run("symlink data directory", func(t *testing.T) {
		parent := t.TempDir()
		target := secureHubDir(t)
		link := filepath.Join(parent, "data")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := EnsureAdminToken(link, ""); err == nil {
			t.Fatal("expected data directory symlink rejection")
		}
	})
}

func TestEnsureAgentTokenUsesSameRawFileContract(t *testing.T) {
	dir := secureHubDir(t)
	first, err := EnsureAgentToken(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnsureAgentToken(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || first != second {
		t.Fatal("agent token was empty or changed")
	}
	path := filepath.Join(dir, "agent-token")
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateTokenFile(path, info); err != nil {
		t.Fatal(err)
	}
}
