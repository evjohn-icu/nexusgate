//go:build !windows

package worker

import (
	"errors"
	"fmt"
	"os"
)

// prepareConfigParent creates the parent directory of the Worker config with
// mode 0700 when it is missing and validates an existing parent. MkdirAll only
// applies the mode to components it creates, so an operator-created parent
// must already be 0700: a Worker config sitting next to a world-traversable
// directory is exactly the exposure the Worker token must not have.
func prepareConfigParent(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect worker config parent: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return errors.New("worker config parent must be a non-symlink directory with mode 0700")
	}
	return nil
}

// validConfigInfo reports whether an existing file may hold Worker config on
// this platform. Symlinks are rejected because the destination the operator
// secured is not the destination the Worker would read through.
func validConfigInfo(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm() == 0o600
}

// validateConfigFile validates an existing destination and tolerates a missing
// file, so enrollment can check the destination before contacting the Hub.
func validateConfigFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect worker config: %w", err)
	}
	if !validConfigInfo(info) {
		return errors.New("worker config must be a regular file with mode 0600")
	}
	return nil
}

// requireConfigFile validates an existing destination and fails when it is
// missing, as loading or verifying a written config must.
func requireConfigFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !validConfigInfo(info) {
		return errors.New("worker config must be a regular file with mode 0600")
	}
	return nil
}

// createConfigTempFile creates a private temporary file inside dir for the
// next config publication. os.CreateTemp already opens with 0600; the chmod
// keeps that invariant explicit so a future temp-template change cannot widen
// it.
func createConfigTempFile(dir string) (*os.File, error) {
	temporary, err := os.CreateTemp(dir, ".worker.json-")
	if err != nil {
		return nil, err
	}
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return nil, err
	}
	return temporary, nil
}

// commitConfigFile publishes the flushed temporary file atomically. Rename
// within the same directory never exposes a partial file at the destination.
func commitConfigFile(temporaryPath, finalPath string) error {
	return os.Rename(temporaryPath, finalPath)
}

// syncConfigDirectory makes the rename itself durable. This is the only step
// that cannot be recovered by the write-ahead state of the two files, so it
// runs on every publication.
func syncConfigDirectory(dir string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		directory.Close()
		return err
	}
	return directory.Close()
}
