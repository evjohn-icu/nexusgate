//go:build !windows

package secretstore

import "os"

func weakenTestSecretFile(path string) error {
	return os.Chmod(path, 0o644)
}
