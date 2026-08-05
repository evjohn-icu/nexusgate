package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/config"
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
