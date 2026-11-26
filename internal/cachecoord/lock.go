// Package cachecoord coordinates cache maintenance with readers across
// processes. Shared locks cover the lifetime of an open artifact.
package cachecoord

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

type Lock struct{ file *os.File }

func AcquireExclusive(dataDir string) (*Lock, error) {
	return AcquirePath(filepath.Join(dataDir, ".cache-maintenance.lock"), true)
}
func AcquireShared(dataDir string) (*Lock, error) {
	return AcquirePath(filepath.Join(dataDir, ".cache-maintenance.lock"), false)
}

// AcquirePath coordinates operations on the resource identified by lock path.
func AcquirePath(path string, exclusive bool) (*Lock, error) {
	return acquire(path, exclusive)
}

// AcquireWaveform keeps a manifest and its named payload stable for readers.
// Callers acquire the shared cache lock first.
func AcquireWaveform(dataDir, assetID string, exclusive bool) (*Lock, error) {
	name := fmt.Sprintf("%x.lock", sha256.Sum256([]byte(assetID)))
	return AcquirePath(filepath.Join(dataDir, ".waveform-locks", name), exclusive)
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
