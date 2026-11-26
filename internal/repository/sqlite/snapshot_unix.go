//go:build !windows

package sqlite

import (
	"os"
	"path/filepath"
)

// publishSnapshotFile publishes the flushed temporary snapshot atomically:
// the rename within the same directory never exposes a partial file at the
// destination.
func publishSnapshotFile(tempPath, snapshotPath string) error {
	if err := os.Rename(tempPath, snapshotPath); err != nil {
		return err
	}
	dir := filepath.Dir(snapshotPath)
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
