//go:build windows

package hubauth

import (
	"fmt"
	"os"

	"github.com/evjohn-icu/nexusgate/internal/winprivate"
)

func prepareTokenDirectory(path string) error {
	if err := winprivate.EnsureDirectory(path); err != nil {
		return fmt.Errorf("protect Hub data directory: %w", err)
	}
	return nil
}

func validateTokenFile(path string, info os.FileInfo) error {
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("token must be a regular file")
	}
	return winprivate.Validate(path, false)
}

func createTokenTemp(dir, prefix string) (*os.File, error) {
	return winprivate.CreateTemp(dir, prefix)
}

func publishToken(source, destination string) error {
	return winprivate.Replace(source, destination)
}
