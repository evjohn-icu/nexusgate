// Package testhelper contains small, platform-neutral helpers shared by
// integration tests that need to execute an external command.
package testhelper

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// InstallCommand builds the repository's fake command and installs a copy in
// dir under name. The fake command selects its deterministic behavior from
// the executable name, so callers exercise the same os/exec and PATH lookup
// paths as production code on every supported platform.
func InstallCommand(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create fake command directory: %v", err)
	}
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	name = name + ext

	buildDir := t.TempDir()
	binary := filepath.Join(buildDir, "timingdex-test-command"+ext)
	root := repositoryRoot(t)
	cmd := exec.Command("go", "build", "-trimpath", "-o", binary, "./internal/testhelper/cmd/fakecommand")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake command: %v: %s", err, out)
	}

	installed := filepath.Join(dir, name)
	source, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("read fake command: %v", err)
	}
	if err := os.WriteFile(installed, source, 0o700); err != nil {
		t.Fatalf("install fake command %q: %v", installed, err)
	}
	return installed
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test helper source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("locate repository root: %v", err)
	}
	return root
}
