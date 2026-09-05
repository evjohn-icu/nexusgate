package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/secretstore"
)

func TestUsage(t *testing.T) {
	err := usage()
	if err == nil || err.Error() != "invalid command" {
		t.Fatalf("usage() error = %v, want %q", err, "invalid command")
	}
}

// captureUsageText runs usage() with stderr redirected and returns what it
// printed. usage() doubles as the no-args error path, so the error itself is
// asserted here too.
func captureUsageText(t *testing.T) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = orig })
	usageErr := usage()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if usageErr == nil || usageErr.Error() != "invalid command" {
		t.Fatalf("usage() error = %v, want %q", usageErr, "invalid command")
	}
	return string(data)
}

func TestUsageListsAllCommands(t *testing.T) {
	text := captureUsageText(t)
	for _, command := range []string{"serve", "root", "pipeline", "reanalyze", "search", "doctor", "secrets", "worker", "cache"} {
		if !strings.Contains(text, "nexusgate "+command) {
			t.Errorf("usage() output does not mention the %q command", command)
		}
	}
}

// TestUsageDocumentsCompleteSubcommands proves the synopsis enumerates every
// implemented subcommand and Worker enrollment flag, not just the top-level
// commands.
func TestUsageDocumentsCompleteSubcommands(t *testing.T) {
	text := captureUsageText(t)
	for _, want := range []string{
		"nexusgate search rebuild",
		"nexusgate search rebuild-embeddings",
		"nexusgate cache inspect",
		"nexusgate cache gc",
		"nexusgate cache verify",
		"nexusgate cache repair-derived",
		"--root",
		"--cache",
		"--config",
		"--fingerprint",
		"--pairing",
		"--hub",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("usage() output missing %q", want)
		}
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
	t.Setenv("NEXUSGATE_WORKER_CONFIG", "/tmp/worker.json")
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
			wantErr: "NEXUSGATE_TLS_CERT_FILE",
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
	os.Args = []string{"nexusgate", "secrets", "rekey"}
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
	t.Setenv("NEXUSGATE_LOG_FORMAT", "json")
	t.Setenv("NEXUSGATE_LOG_LEVEL", "debug")
	setupLogging()
	if !strings.Contains(fmt.Sprintf("%T", slog.Default().Handler()), "JSONHandler") {
		t.Fatalf("handler = %T, want JSONHandler", slog.Default().Handler())
	}
}

func TestSetupLoggingTextDefault(t *testing.T) {
	t.Setenv("NEXUSGATE_LOG_FORMAT", "text")
	t.Setenv("NEXUSGATE_LOG_LEVEL", "info")
	setupLogging()
	if strings.Contains(fmt.Sprintf("%T", slog.Default().Handler()), "JSONHandler") {
		t.Fatalf("handler = %T, want TextHandler for text format", slog.Default().Handler())
	}
}

func TestSetupLoggingInvalidLevelFallsBackToInfo(t *testing.T) {
	t.Setenv("NEXUSGATE_LOG_FORMAT", "text")
	t.Setenv("NEXUSGATE_LOG_LEVEL", "bogus")
	setupLogging() // must not panic; falls back to info with a warning
	if got := slog.Default().Enabled(context.Background(), slog.LevelDebug); got {
		t.Fatalf("invalid level fell back to debug instead of info")
	}
}

func TestSetupLoggingInvalidFormatFallsBackToText(t *testing.T) {
	t.Setenv("NEXUSGATE_LOG_FORMAT", "bogus")
	t.Setenv("NEXUSGATE_LOG_LEVEL", "debug")
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
		{name: "no args", args: []string{"nexusgate"}, wantErr: "invalid command"},
		{name: "unknown subcommand", args: []string{"nexusgate", "bogus"}, wantErr: "invalid command"},
		{name: "worker without subcommand", args: []string{"nexusgate", "worker"}, wantErr: "usage: nexusgate worker enroll|run|doctor"},
		{name: "worker unknown subcommand", args: []string{"nexusgate", "worker", "bogus"}, wantErr: "usage: nexusgate worker enroll|run|doctor"},
		{name: "root without subcommand", args: []string{"nexusgate", "root"}, wantErr: "usage: nexusgate root add|list|scan"},
		{name: "root unknown subcommand", args: []string{"nexusgate", "root", "bogus"}, wantErr: "usage: nexusgate root add|list|scan"},
		{name: "pipeline without subcommand", args: []string{"nexusgate", "pipeline"}, wantErr: "usage: nexusgate pipeline run|retry-failed"},
		{name: "pipeline unknown subcommand", args: []string{"nexusgate", "pipeline", "bogus"}, wantErr: "usage: nexusgate pipeline run|retry-failed"},
		{name: "secrets without subcommand", args: []string{"nexusgate", "secrets"}, wantErr: "usage: nexusgate secrets rekey"},
		{name: "secrets unknown subcommand", args: []string{"nexusgate", "secrets", "bogus"}, wantErr: "usage: nexusgate secrets rekey"},
		{name: "serve invalid flag", args: []string{"nexusgate", "serve", "-bogus"}, wantErr: "flag provided but not defined"},
		{name: "worker enroll invalid flag", args: []string{"nexusgate", "worker", "enroll", "-bogus"}, wantErr: "flag provided but not defined"},
		{name: "worker run invalid flag", args: []string{"nexusgate", "worker", "run", "-bogus"}, wantErr: "flag provided but not defined"},
		{name: "worker doctor invalid flag", args: []string{"nexusgate", "worker", "doctor", "-bogus"}, wantErr: "flag provided but not defined"},
		{name: "doctor", args: []string{"nexusgate", "doctor"}},
		{name: "root list empty", args: []string{"nexusgate", "root", "list"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := secureTestDataDir(t)
			t.Setenv("NEXUSGATE_DATA_DIR", dataDir)
			t.Setenv("NEXUSGATE_TLS_MODE", "off")    // avoid auto-generating self-signed certs
			t.Setenv("NEXUSGATE_LOG_LEVEL", "error") // suppress info output
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
	dataDir := secureTestDataDir(t)
	t.Setenv("NEXUSGATE_DATA_DIR", dataDir)
	t.Setenv("NEXUSGATE_TLS_MODE", "files")
	t.Setenv("NEXUSGATE_TLS_CERT_FILE", filepath.Join(dataDir, "cert.pem"))
	t.Setenv("NEXUSGATE_TLS_KEY_FILE", filepath.Join(dataDir, "key.pem"))
	t.Setenv("NEXUSGATE_LOG_LEVEL", "error")
	setArgs(t, []string{"nexusgate", "serve"})
	err := run()
	if err == nil {
		t.Fatal("expected error from missing TLS files")
	}
	t.Logf("serve with missing TLS files returned: %v", err)
}

// TestDoctorReportsInvalidProviderConfig proves `doctor` stays runnable when
// the legacy providers.* config is broken (an enabled selected Gemini block
// with GEMINI_API_KEY unset): it must emit every Doctor section plus the
// secret-free provider diagnosis, while an operational command (`serve`) still
// fails fast before any service work.
func TestDoctorReportsInvalidProviderConfig(t *testing.T) {
	dataDir := secureTestDataDir(t)
	t.Setenv("NEXUSGATE_DATA_DIR", dataDir)
	t.Setenv("NEXUSGATE_TLS_MODE", "off")
	t.Setenv("NEXUSGATE_LOG_LEVEL", "error")
	t.Setenv("GEMINI_API_KEY", "") // deliberately unset
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(`{"providers":{"vision_primary":"gemini","gemini":{"enabled":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// doctor must run to completion and print every section plus the reason.
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = origStdout })
	setArgs(t, []string{"nexusgate", "doctor"})
	runErr := run()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		t.Fatalf("doctor must not fail on an invalid provider config: %v", runErr)
	}
	out := string(data)
	for _, section := range []string{"SYSTEM", "DB", "STORAGE", "MEDIA ROOTS", "FFMPEG", "FFPROBE", "EXIFTOOL", "GPU", "PROVIDERS", "SEARCH INDEX", "WORKERS", "QUEUE", "TEMP FILES"} {
		if !strings.Contains(out, section) {
			t.Errorf("doctor output missing section %q", section)
		}
	}
	if !strings.Contains(out, "✗ legacy provider config:") {
		t.Errorf("doctor output missing the legacy provider config diagnosis")
	}
	if !strings.Contains(out, `"GEMINI_API_KEY"`) {
		t.Errorf("doctor output missing the missing-variable diagnosis")
	}

	// An operational command must still fail before any service work.
	setArgs(t, []string{"nexusgate", "serve"})
	err = run()
	if err == nil || !strings.Contains(err.Error(), "invalid provider config") {
		t.Fatalf("serve with invalid provider config = %v, want invalid provider config failure", err)
	}
}
