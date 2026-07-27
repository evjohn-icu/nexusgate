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
	if token := strings.TrimSpace(explicit); token != "" {
		return token, nil
	}
	if strings.TrimSpace(dataDir) == "" {
		return randomToken()
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("create Hub data directory: %w", err)
	}
	path := filepath.Join(dataDir, "admin-token")
	if raw, err := os.ReadFile(path); err == nil {
		token := strings.TrimSpace(string(raw))
		if token == "" {
			return "", fmt.Errorf("Hub administrator token is empty")
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return "", fmt.Errorf("secure Hub administrator token: %w", err)
		}
		return token, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read Hub administrator token: %w", err)
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(dataDir, ".admin-token-")
	if err != nil {
		return "", fmt.Errorf("create Hub administrator token: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return "", fmt.Errorf("secure Hub administrator token: %w", err)
	}
	if _, err := temporary.WriteString(token + "\n"); err != nil {
		temporary.Close()
		return "", fmt.Errorf("write Hub administrator token: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return "", fmt.Errorf("sync Hub administrator token: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close Hub administrator token: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", fmt.Errorf("commit Hub administrator token: %w", err)
	}
	return token, nil
}

func randomToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate Hub administrator token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}
