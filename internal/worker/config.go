package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/evjohn-icu/timingdex/internal/remote"
)

// Config intentionally contains only the Hub-issued node token. Provider
// credentials are never persisted on a Worker.
type Config struct {
	HubURL                 string                    `json:"hub_url"`
	CertificateFingerprint string                    `json:"certificate_fingerprint"`
	Token                  string                    `json:"token"`
	CacheDir               string                    `json:"cache_dir,omitempty"`
	Mounts                 map[string]string         `json:"mounts,omitempty"`
	Registration           remote.WorkerRegistration `json:"registration"`
}

func SaveConfig(path string, config Config) error {
	if strings.TrimSpace(config.HubURL) == "" || strings.TrimSpace(config.Token) == "" {
		return fmt.Errorf("worker Hub URL and token are required")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect worker config directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("worker config parent must be a non-symlink directory with mode 0700")
	}
	if existing, err := os.Lstat(path); err == nil {
		if !existing.Mode().IsRegular() || existing.Mode()&os.ModeSymlink != 0 || existing.Mode().Perm() != 0o600 {
			return fmt.Errorf("worker config must be a regular file with mode 0600")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect worker config: %w", err)
	}
	raw, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(dir, ".worker.json-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(raw); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	info, err = os.Lstat(path)
	if err != nil {
		return fmt.Errorf("verify worker config: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("worker config must be a regular file with mode 0600")
	}
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

func LoadConfig(path string) (Config, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Config{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return Config{}, fmt.Errorf("worker config must be a regular file with mode 0600")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var config Config
	if err := json.Unmarshal(raw, &config); err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(config.HubURL) == "" || strings.TrimSpace(config.Token) == "" {
		return Config{}, fmt.Errorf("worker config is incomplete")
	}
	return config, nil
}

// ResolveSourcePath maps the portable Hub locator to a path mounted on this
// Worker. The Hub never sends its NAS absolute path to the Worker.
func (c Config) ResolveSourcePath(rootID, relativePath string) (string, error) {
	root := strings.TrimSpace(c.Mounts[rootID])
	if root == "" {
		return "", fmt.Errorf("worker library root %q is not mounted", rootID)
	}
	if filepath.IsAbs(relativePath) {
		return "", fmt.Errorf("worker source path must be relative: %q", relativePath)
	}
	clean := filepath.Clean(filepath.FromSlash(relativePath))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("worker source path escapes mounted root: %q", relativePath)
	}
	return filepath.Join(root, clean), nil
}

func (c Config) CachePath() string {
	if value := strings.TrimSpace(c.CacheDir); value != "" {
		return value
	}
	return filepath.Join(os.TempDir(), "timingdex-worker-cache")
}
