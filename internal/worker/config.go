package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ev/timingdex/internal/remote"
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func LoadConfig(path string) (Config, error) {
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
