// Package cachecoord coordinates cache maintenance with readers across
// processes. Shared locks cover the lifetime of an open artifact.
package cachecoord

import (
	"fmt"
	"os"
	"path/filepath"
)

type Lock struct{ file *os.File }

func AcquireExclusive(dataDir string) (*Lock, error) {
	return acquire(filepath.Join(dataDir, ".cache-maintenance.lock"), true)
}
func AcquireShared(dataDir string) (*Lock, error) {
	return acquire(filepath.Join(dataDir, ".cache-maintenance.lock"), false)
}
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	if err := unlock(l.file); err != nil {
		_ = l.file.Close()
		return fmt.Errorf("unlock cache maintenance: %w", err)
	}
	return l.file.Close()
}
