//go:build !windows

package worker

import (
	"os"
	"testing"
)

func makeExistingConfigInsecure(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
}

// assertConfigStillSecure asserts the exact state LoadConfig accepts after a
// local-admin update: a regular file with mode 0600.
func assertConfigStillSecure(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !validConfigInfo(info) {
		t.Fatalf("mode/type=%o/%v", info.Mode().Perm(), info.Mode().IsRegular())
	}
}
