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
