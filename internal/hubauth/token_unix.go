//go:build !windows

package hubauth

import (
	"fmt"
	"os"
	"path/filepath"
)

func prepareTokenDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create Hub data directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect Hub data directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("Hub data directory must be a non-symlink directory with mode 0700")
	}
	return nil
}

func validateTokenFile(_ string, info os.FileInfo) error {
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("token must be a regular file with mode 0600")
	}
	return nil
}

func createTokenTemp(dir, pattern string) (*os.File, error) {
	return os.CreateTemp(dir, pattern)
}

func publishToken(source, destination string) error {
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return fmt.Errorf("open Hub data directory: %w", err)
	}
	if err := dir.Sync(); err != nil {
		dir.Close()
		return fmt.Errorf("sync Hub data directory: %w", err)
	}
	return dir.Close()
}
