// Package staging keeps an optional, disposable local copy of a source file
// before media processing. It is designed for mounted NAS folders: source
// files remain read-only and all processing happens on the local machine.
package staging

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Mode string

const (
	ModeNone Mode = "none"
	ModeCopy Mode = "copy"
)

type SourceStager struct {
	mode     Mode
	cacheDir string
}

func New(mode, cacheDir string) (*SourceStager, error) {
	selected := Mode(strings.TrimSpace(strings.ToLower(mode)))
	if selected == "" {
		selected = ModeNone
	}
	if selected != ModeNone && selected != ModeCopy {
		return nil, fmt.Errorf("unsupported source staging mode %q", mode)
	}
	if selected == ModeCopy && strings.TrimSpace(cacheDir) == "" {
		return nil, fmt.Errorf("source staging cache directory is required for copy mode")
	}
	return &SourceStager{mode: selected, cacheDir: cacheDir}, nil
}

// Stage returns sourcePath unchanged in none mode. Copy mode writes a local,
// immutable-by-convention cache entry scoped by assetID and inputVersion. The
// source itself is only opened for reading.
func (s *SourceStager) Stage(ctx context.Context, sourcePath, assetID, inputVersion string) (string, error) {
	if s == nil || s.mode == ModeNone {
		return sourcePath, nil
	}
	if err := validateSegment("asset ID", assetID); err != nil {
		return "", err
	}
	if err := validateSegment("input version", inputVersion); err != nil {
		return "", err
	}
	ext := filepath.Ext(sourcePath)
	if ext == "" {
		ext = ".media"
	}
	destination := filepath.Join(s.cacheDir, "sources", assetID, inputVersion+ext)
	if cached, err := os.Stat(destination); err == nil && cached.Mode().IsRegular() {
		return destination, nil
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		return "", fmt.Errorf("stat source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("source is not a regular file: %s", sourcePath)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return "", fmt.Errorf("create source cache directory: %w", err)
	}

	source, err := os.Open(sourcePath)
	if err != nil {
		return "", fmt.Errorf("open source: %w", err)
	}
	defer source.Close()
	temporary, err := os.CreateTemp(filepath.Dir(destination), "."+inputVersion+"-*.partial")
	if err != nil {
		return "", fmt.Errorf("create staged source: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if err := copyWithContext(ctx, temporary, source); err != nil {
		temporary.Close()
		return "", fmt.Errorf("stage source locally: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return "", fmt.Errorf("sync staged source: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close staged source: %w", err)
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return "", fmt.Errorf("publish staged source: %w", err)
	}
	return destination, nil
}

func validateSegment(label, value string) error {
	if value == "" || value == "." || value == ".." || filepath.Base(value) != value || strings.ContainsRune(value, filepath.Separator) {
		return fmt.Errorf("invalid source staging %s %q", label, value)
	}
	return nil
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader) error {
	buffer := make([]byte, 1024*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			if _, err := destination.Write(buffer[:count]); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
