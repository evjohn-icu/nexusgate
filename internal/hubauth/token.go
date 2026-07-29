package hubauth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnsureAdminToken returns an explicit deployment token or creates the Hub's
// local administrator token once. The generated token is never logged or
// stored in SQLite; it is available only to the Hub process and its mode-0600
// data-dir file.
func EnsureAdminToken(dataDir, explicit string) (string, error) {
	return ensureToken(dataDir, "admin-token", "Hub administrator", explicit)
}

// EnsureAgentToken returns an explicit deployment token or creates the Hub's
// local agent token once. It follows the exact same on-disk contract as
// EnsureAdminToken (explicit-override support; atomic CreateTemp + Chmod 0600
// + Sync + Rename; never logged or stored in SQLite; mode-0600 data-dir
// file) but is a distinct credential from the admin token: it authorizes only
// the narrow, human_required agent surface enforced by requireAgentOrAdmin in
// internal/api, never the full admin surface EnsureAdminToken guards.
func EnsureAgentToken(dataDir, explicit string) (string, error) {
	return ensureToken(dataDir, "agent-token", "Hub agent", explicit)
}

// ensureToken is the shared implementation behind EnsureAdminToken and
// EnsureAgentToken. An explicit override always wins; otherwise a token is
// read from <dataDir>/<filename> if present, or generated and persisted
// atomically on first run. label is used only in error text.
func ensureToken(dataDir, filename, label, explicit string) (string, error) {
	if token := strings.TrimSpace(explicit); token != "" {
		return token, nil
	}
	if strings.TrimSpace(dataDir) == "" {
		return randomToken(label)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("create Hub data directory: %w", err)
	}
	path := filepath.Join(dataDir, filename)
	if raw, err := os.ReadFile(path); err == nil {
		token := strings.TrimSpace(string(raw))
		if token == "" {
			return "", fmt.Errorf("%s token is empty", label)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return "", fmt.Errorf("secure %s token: %w", label, err)
		}
		return token, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read %s token: %w", label, err)
	}
	token, err := randomToken(label)
	if err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(dataDir, "."+filename+"-")
	if err != nil {
		return "", fmt.Errorf("create %s token: %w", label, err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return "", fmt.Errorf("secure %s token: %w", label, err)
	}
	if _, err := temporary.WriteString(token + "\n"); err != nil {
		temporary.Close()
		return "", fmt.Errorf("write %s token: %w", label, err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return "", fmt.Errorf("sync %s token: %w", label, err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close %s token: %w", label, err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", fmt.Errorf("commit %s token: %w", label, err)
	}
	return token, nil
}

func randomToken(label string) (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate %s token: %w", label, err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}
