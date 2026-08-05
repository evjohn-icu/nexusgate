package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/secretstore"
)

func TestUsage(t *testing.T) {
	err := usage()
	if err == nil || err.Error() != "invalid command" {
		t.Fatalf("usage() error = %v, want %q", err, "invalid command")
	}
}

func TestParseWorkerMounts(t *testing.T) {
	clean := func(p string) string { return filepath.Clean(p) }
	tests := []struct {
		name    string
		values  []string
		want    map[string]string
		wantErr string
	}{
		{name: "nil values", want: map[string]string{}},
		{name: "single", values: []string{"root-1=/media/disk"}, want: map[string]string{"root-1": clean("/media/disk")}},
		{name: "multiple", values: []string{"root-1=/a", "root-2=/b"}, want: map[string]string{"root-1": clean("/a"), "root-2": clean("/b")}},
		{name: "trims surrounding spaces", values: []string{" root-1 = /a "}, want: map[string]string{"root-1": clean("/a")}},
		{name: "no equals separator", values: []string{"root-1"}, wantErr: "invalid --mount"},
		{name: "empty root id", values: []string{"=/a"}, wantErr: "invalid --mount"},
		{name: "empty local path", values: []string{"root-1="}, wantErr: "invalid --mount"},
		{name: "duplicate same mapping accepted", values: []string{"r=/a", "r=/a"}, want: map[string]string{"r": clean("/a")}},
		{name: "duplicate conflicting mapping rejected", values: []string{"r=/a", "r=/b"}, wantErr: "mapped more than once"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseWorkerMounts(tc.values)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseWorkerMounts(%v) error = %v, want containing %q", tc.values, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseWorkerMounts(%v) unexpected error: %v", tc.values, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseWorkerMounts(%v) = %v, want %v", tc.values, got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("parseWorkerMounts(%v) = %v, want %v", tc.values, got, tc.want)
				}
			}
		})
	}
}

func TestRepeatedFlag(t *testing.T) {
	var f repeatedFlag
	if got := f.String(); got != "" {
		t.Fatalf("empty repeatedFlag String() = %q, want empty", got)
	}
	if err := f.Set("a"); err != nil {
		t.Fatalf("Set(a) error: %v", err)
	}
	if err := f.Set("b"); err != nil {
		t.Fatalf("Set(b) error: %v", err)
	}
	if got := f.String(); got != "a,b" {
		t.Fatalf("repeatedFlag String() = %q, want %q", got, "a,b")
	}
}

func TestDefaultWorkerConfigPathEnvOverride(t *testing.T) {
	t.Setenv("TIMINGDEX_WORKER_CONFIG", "/tmp/worker.json")
	if got := defaultWorkerConfigPath(); got != "/tmp/worker.json" {
		t.Fatalf("defaultWorkerConfigPath() with env = %q, want %q", got, "/tmp/worker.json")
	}
}

func TestHostnameNonEmpty(t *testing.T) {
	if got := hostname(); got == "" {
		t.Fatal("hostname() returned empty string")
	}
}

func TestHubTLSFilesModes(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.Config
		want    string
		wantErr string
	}{
		{name: "off returns empty paths", cfg: config.Config{HubTLS: config.HubTLSConfig{Mode: "off"}}},
		{
			name:    "files mode requires cert and key",
			cfg:     config.Config{HubTLS: config.HubTLSConfig{Mode: "files"}},
			wantErr: "TIMINGDEX_TLS_CERT_FILE",
		},
		{
			name: "files mode returns configured paths",
			cfg: config.Config{HubTLS: config.HubTLSConfig{
				Mode:            "files",
				CertificateFile: "/etc/tls/cert.pem",
				KeyFile:         "/etc/tls/key.pem",
			}},
			want: "/etc/tls/cert.pem",
		},
		{name: "unsupported mode rejected", cfg: config.Config{HubTLS: config.HubTLSConfig{Mode: "bogus"}}, wantErr: "unsupported Hub TLS mode"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cert, key, err := hubTLSFiles(tc.cfg)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("hubTLSFiles() error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("hubTLSFiles() unexpected error: %v", err)
			}
			if tc.want != "" && cert != tc.want {
				t.Fatalf("hubTLSFiles() cert = %q, want %q", cert, tc.want)
			}
			if tc.cfg.HubTLS.Mode == "off" && (cert != "" || key != "") {
				t.Fatalf("hubTLSFiles() off mode returned %q/%q, want empty", cert, key)
			}
		})
	}
}

func TestRunSecretsCommandRekeySmoke(t *testing.T) {
	dataDir := t.TempDir()
	const adminToken = "cli-smoke-admin-token"

	// Seed a store with one secret, the same way Service construction would.
	store, err := secretstore.Open(dataDir, adminToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put("provider-channel/c1/m1", "sk-smoke"); err != nil {
		t.Fatal(err)
	}

	keyFile := filepath.Join(dataDir, "provider-secrets", "store.key")
	before, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}

	// Drive the CLI rekey path with an explicit token (config.AdminToken is
	// environment-only and empty by default, so EnsureAdminToken falls through
	// to data-dir creation — the explicit path is what a real operator uses).
	cfg := config.Config{DataDir: dataDir, HubSecurity: config.HubSecurityConfig{AdminToken: adminToken}}
	origArgs := os.Args
	os.Args = []string{"timingdex", "secrets", "rekey"}
	t.Cleanup(func() { os.Args = origArgs })
	if err := runSecretsCommand(cfg); err != nil {
		t.Fatalf("runSecretsCommand(rekey) = %v", err)
	}

	after, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) == string(after) {
		t.Fatal("store.key did not change after CLI rekey")
	}

	// Backup of the previous key must exist and hold the pre-rekey key.
	backupPath := filepath.Join(dataDir, "provider-secrets", "store.key.pre-rekey")
	backup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("pre-rekey backup missing: %v", err)
	}
	if string(backup) != string(before) {
		t.Fatal("pre-rekey backup does not hold the original key")
	}

	// The secret must still resolve with the same ref.
	reopened, err := secretstore.Open(dataDir, adminToken)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok, err := reopened.Resolve("provider-channel/c1/m1"); err != nil || !ok || got != "sk-smoke" {
		t.Fatalf("secret after CLI rekey = %q, %t, %v; want sk-smoke", got, ok, err)
	}
}

func TestSetupLoggingJSONFormatAndLevel(t *testing.T) {
	t.Setenv("TIMINGDEX_LOG_FORMAT", "json")
	t.Setenv("TIMINGDEX_LOG_LEVEL", "debug")
	setupLogging()
	if !strings.Contains(fmt.Sprintf("%T", slog.Default().Handler()), "JSONHandler") {
		t.Fatalf("handler = %T, want JSONHandler", slog.Default().Handler())
	}
}

func TestSetupLoggingTextDefault(t *testing.T) {
	t.Setenv("TIMINGDEX_LOG_FORMAT", "text")
	t.Setenv("TIMINGDEX_LOG_LEVEL", "info")
	setupLogging()
	if strings.Contains(fmt.Sprintf("%T", slog.Default().Handler()), "JSONHandler") {
		t.Fatalf("handler = %T, want TextHandler for text format", slog.Default().Handler())
	}
}

func TestSetupLoggingInvalidLevelFallsBackToInfo(t *testing.T) {
	t.Setenv("TIMINGDEX_LOG_FORMAT", "text")
	t.Setenv("TIMINGDEX_LOG_LEVEL", "bogus")
	setupLogging() // must not panic; falls back to info with a warning
	if got := slog.Default().Enabled(context.Background(), slog.LevelDebug); got {
		t.Fatalf("invalid level fell back to debug instead of info")
	}
}

func TestSetupLoggingInvalidFormatFallsBackToText(t *testing.T) {
	t.Setenv("TIMINGDEX_LOG_FORMAT", "bogus")
	t.Setenv("TIMINGDEX_LOG_LEVEL", "debug")
	setupLogging() // must not panic; falls back to text with a warning
	if strings.Contains(fmt.Sprintf("%T", slog.Default().Handler()), "JSONHandler") {
		t.Fatalf("invalid format fell back to JSONHandler instead of TextHandler")
	}
}

func TestTruncateEnv(t *testing.T) {
	tests := []struct {
		name   string
		envVal string
		maxLen int
		want   string
	}{
		{name: "empty", envVal: "", maxLen: 20, want: ""},
		{name: "short enough", envVal: "debug", maxLen: 20, want: "debug"},
		{name: "exactly max", envVal: "12345678901234567890", maxLen: 20, want: "12345678901234567890"},
		{name: "too long truncated", envVal: "1234567890123456789012345", maxLen: 20, want: "12345678901234567890..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TEST_TRUNCATE", tt.envVal)
			got := truncateEnv("TEST_TRUNCATE", tt.maxLen)
			if got != tt.want {
				t.Fatalf("truncateEnv() = %q, want %q", got, tt.want)
			}
		})
	}
}

// setArgs replaces os.Args for the duration of a test. Tests that call setArgs
// must not run in parallel.
func setArgs(t *testing.T, args []string) {
	t.Helper()
	orig := os.Args
	os.Args = args
	t.Cleanup(func() { os.Args = orig })
}

func TestRunSubcommandDispatch(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
		// env sets additional environment variables for this sub-test.
		env map[string]string
	}{
		{name: "no args", args: []string{"timingdex"}, wantErr: "invalid command"},
		{name: "unknown subcommand", args: []string{"timingdex", "bogus"}, wantErr: "invalid command"},
		{name: "worker without subcommand", args: []string{"timingdex", "worker"}, wantErr: "usage: timingdex worker enroll|run|doctor"},
		{name: "worker unknown subcommand", args: []string{"timingdex", "worker", "bogus"}, wantErr: "usage: timingdex worker enroll|run|doctor"},
		{name: "root without subcommand", args: []string{"timingdex", "root"}, wantErr: "usage: timingdex root add|list|scan"},
		{name: "root unknown subcommand", args: []string{"timingdex", "root", "bogus"}, wantErr: "usage: timingdex root add|list|scan"},
		{name: "pipeline without subcommand", args: []string{"timingdex", "pipeline"}, wantErr: "usage: timingdex pipeline run|retry-failed"},
		{name: "pipeline unknown subcommand", args: []string{"timingdex", "pipeline", "bogus"}, wantErr: "usage: timingdex pipeline run|retry-failed"},
		{name: "secrets without subcommand", args: []string{"timingdex", "secrets"}, wantErr: "usage: timingdex secrets rekey"},
		{name: "secrets unknown subcommand", args: []string{"timingdex", "secrets", "bogus"}, wantErr: "usage: timingdex secrets rekey"},
		{name: "serve invalid flag", args: []string{"timingdex", "serve", "-bogus"}, wantErr: "flag provided but not defined"},
		{name: "worker enroll invalid flag", args: []string{"timingdex", "worker", "enroll", "-bogus"}, wantErr: "flag provided but not defined"},
		{name: "worker run invalid flag", args: []string{"timingdex", "worker", "run", "-bogus"}, wantErr: "flag provided but not defined"},
		{name: "worker doctor invalid flag", args: []string{"timingdex", "worker", "doctor", "-bogus"}, wantErr: "flag provided but not defined"},
		{name: "doctor", args: []string{"timingdex", "doctor"}},
		{name: "root list empty", args: []string{"timingdex", "root", "list"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := t.TempDir()
			t.Setenv("TIMINGDEX_DATA_DIR", dataDir)
			t.Setenv("TIMINGDEX_TLS_MODE", "off")    // avoid auto-generating self-signed certs
			t.Setenv("TIMINGDEX_LOG_LEVEL", "error") // suppress info output
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			setArgs(t, tt.args)
			err := run()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("run() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("run() unexpected error: %v", err)
			}
		})
	}
}

func TestRunServeDispatchTLSFilesMissing(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("TIMINGDEX_DATA_DIR", dataDir)
	t.Setenv("TIMINGDEX_TLS_MODE", "files")
	t.Setenv("TIMINGDEX_TLS_CERT_FILE", filepath.Join(dataDir, "cert.pem"))
	t.Setenv("TIMINGDEX_TLS_KEY_FILE", filepath.Join(dataDir, "key.pem"))
	t.Setenv("TIMINGDEX_LOG_LEVEL", "error")
	setArgs(t, []string{"timingdex", "serve"})
	err := run()
	if err == nil {
		t.Fatal("expected error from missing TLS files")
	}
	t.Logf("serve with missing TLS files returned: %v", err)
}

func TestRunRootAdd(t *testing.T) {
	dataDir := t.TempDir()
	rootPath := filepath.Join(dataDir, "footage")
	if err := os.MkdirAll(rootPath, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TIMINGDEX_DATA_DIR", dataDir)
	t.Setenv("TIMINGDEX_TLS_MODE", "off")
	t.Setenv("TIMINGDEX_LOG_LEVEL", "error")
	setArgs(t, []string{"timingdex", "root", "add", rootPath})
	err := run()
	if err != nil {
		t.Fatalf("root add: %v", err)
	}
	// Verify the root was persisted.
	setArgs(t, []string{"timingdex", "root", "list"})
	// Reconstruct because run() reuses os.Args; we need a fresh setup.
	// run() opens the DB again, which is fine.
	// But run() calls config.Load() which reads TIMINGDEX_DATA_DIR.
	// That's still set from t.Setenv above.
	origArgs := os.Args
	os.Args = []string{"timingdex", "root", "list"}
	defer func() { os.Args = origArgs }()
	if err := run(); err != nil {
		t.Fatalf("root list after add: %v", err)
	}
}
