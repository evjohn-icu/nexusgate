//go:build windows

package secretstore

import (
	"os"

	"github.com/evjohn-icu/nexusgate/internal/winprivate"
)

func defaultFileOps() fileOps {
	return fileOps{
		createTemp: func(dir string) (*os.File, error) { return winprivate.CreateTemp(dir, ".provider-secrets-") },
		rename:     winprivate.Replace, syncDirectory: syncDirectory, remove: os.Remove,
	}
}

func ensurePrivateDirectory(path string) error {
	return winprivate.EnsureDirectory(path)
}

func validateSecretDirectory(path string, _ os.FileInfo) error {
	return winprivate.Validate(path, true)
}

func validateSecretFilePermissions(path string, _ os.FileInfo) error {
	return winprivate.Validate(path, false)
}

// Write-through replacement already publishes the directory change.
func syncDirectory(_ string) error {
	return nil
}
