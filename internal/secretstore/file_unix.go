//go:build !windows

package secretstore

import (
	"fmt"
	"os"
)

func defaultFileOps() fileOps {
	return fileOps{createTemp: createSecretTemp, rename: os.Rename, syncDirectory: syncDirectory, remove: os.Remove}
}

func createSecretTemp(dir string) (*os.File, error) {
	file, err := os.CreateTemp(dir, ".provider-secrets-*")
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(secretFileMode); err != nil {
		file.Close()
		os.Remove(file.Name())
		return nil, err
	}
	return file, nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, dataDirMode); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return validateSecretDirectory(path, info)
}

func validateSecretDirectory(_ string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("data directory is not a private directory")
	}
	if info.Mode().Perm() != dataDirMode {
		return fmt.Errorf("data directory must have mode 0700")
	}
	return nil
}

func validateSecretFilePermissions(_ string, info os.FileInfo) error {
	if info.Mode().Perm() != secretFileMode {
		return fmt.Errorf("secret file must have mode 0600")
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}
