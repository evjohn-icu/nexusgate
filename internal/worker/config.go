package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/evjohn-icu/nexusgate/internal/remote"
)

// Config intentionally contains only the Hub-issued node token. Provider
// credentials are never persisted on a Worker.
type Config struct {
	HubURL                 string `json:"hub_url"`
	CertificateFingerprint string `json:"certificate_fingerprint"`
	Token                  string `json:"token"`
	CacheDir               string `json:"cache_dir,omitempty"`
	// SourceCacheMaxBytes caps cache/sources on this node. It is a pointer so
	// three states stay distinct: absent takes DefaultSourceCacheMaxBytes, an
	// explicit 0 means unbounded, and anything else is the operator's number.
	// A plain int64 would collapse "not set" into "unbounded", which is the
	// one answer a Worker must not default to — this side stages in copy mode
	// unconditionally (see runtime.go), so unlike the Hub nobody ever opted
	// into it and nobody was ever asked how large it may grow.
	SourceCacheMaxBytes *int64            `json:"source_cache_max_bytes,omitempty"`
	Mounts              map[string]string `json:"mounts,omitempty"`
	// PreviewLUTPath is resolved on this Worker's filesystem, not the Hub's, so
	// it must live in worker.json instead of being pushed down: Workers render
	// proxy clips too and need the LUT to grade Log footage into a safe SDR
	// preview. A missing file is not an error here — media.ResolvePlan reports
	// "needs readable LUT" per job with the path. Not a credential.
	PreviewLUTPath string                    `json:"preview_lut_path,omitempty"`
	Registration   remote.WorkerRegistration `json:"registration"`
}

// DefaultSourceCacheMaxBytes is the cap a Worker gets when its config does not
// name one. 256 GiB is sized from what this node does with the directory
// rather than from how much disk it might have: a Worker executes derive jobs
// only (runtime.go rejects every other type), one at a time, so a staged copy
// is read once and never again. There is no working set to preserve — the
// cache is transit, and any byte past "comfortably holds the largest single
// clip" buys nothing. The Hub is the opposite and defaults to unbounded:
// sourcePath runs before the job-type switch, so probe, derive, speech_gate,
// transcribe and analyze all read one staged copy, and evicting it early costs
// a re-read of the share.
//
// The number comes from the largest realistic single source. Apple Log is
// ProRes, and ProRes 422 HQ at 4K60 runs about 12 GB per minute, so a
// twenty-minute continuous take is roughly 240 GB. A cap below the biggest
// clip still works — eviction clears what it can and stages anyway — but it
// warns on every stage, which is noise rather than information.
const DefaultSourceCacheMaxBytes int64 = 256 << 30

// SourceCacheCap resolves the three states of SourceCacheMaxBytes.
func (c Config) SourceCacheCap() int64 {
	if c.SourceCacheMaxBytes == nil {
		return DefaultSourceCacheMaxBytes
	}
	return *c.SourceCacheMaxBytes
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
	return filepath.Join(os.TempDir(), "nexusgate-worker-cache")
}
