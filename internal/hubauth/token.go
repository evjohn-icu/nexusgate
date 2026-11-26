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
// local administrator token once. The Hub keeps the generated token in a
// private file under its data directory and keeps it out of logs and SQLite.
func EnsureAdminToken(dataDir, explicit string) (string, error) {
	return ensureToken(dataDir, "admin-token", "Hub administrator", explicit)
}

// EnsureAgentToken returns an explicit deployment token or creates the Hub's
// local agent token once. It shares the private on-disk storage and explicit
// override behavior of EnsureAdminToken, but accepts only requests within
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
	if err := prepareTokenDirectory(dataDir); err != nil {
		return "", err
	}
	path := filepath.Join(dataDir, filename)
	if info, err := os.Lstat(path); err == nil {
		if err := validateTokenFile(path, info); err != nil {
			return "", fmt.Errorf("validate %s token: %w", label, err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read %s token: %w", label, err)
		}
		token := strings.TrimSpace(string(raw))
		if token == "" {
			return "", fmt.Errorf("%s token is empty", label)
		}
		return token, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read %s token: %w", label, err)
	}
	token, err := randomToken(label)
	if err != nil {
		return "", err
	}
	temporary, err := createTokenTemp(dataDir, "."+filename+"-")
	if err != nil {
		return "", fmt.Errorf("create %s token: %w", label, err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
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
	if err := publishToken(temporaryPath, path); err != nil {
		return "", fmt.Errorf("commit %s token: %w", label, err)
	}
	info, err := os.Lstat(path)
	if err == nil {
		err = validateTokenFile(path, info)
	}
	if err != nil {
		return "", fmt.Errorf("verify %s token: %w", label, err)
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
