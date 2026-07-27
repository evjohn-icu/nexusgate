package worker

import (
	"path/filepath"
	"testing"

	"github.com/ev/timingdex/internal/remote"
)

func TestConfigRoundTripKeepsNodeTokenButNotProviderSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.json")
	want := Config{HubURL: "https://nas.local:8787", CertificateFingerprint: "abcd", Token: "node-token", CacheDir: filepath.Join(t.TempDir(), "cache"), Mounts: map[string]string{"nas-main": filepath.Join(t.TempDir(), "media")}, Registration: remote.WorkerRegistration{Name: "rk3566", Platform: "linux-arm64"}}
	if err := SaveConfig(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != want.Token || got.HubURL != want.HubURL || got.Registration.Platform != "linux-arm64" || got.CacheDir != want.CacheDir || got.Mounts["nas-main"] != want.Mounts["nas-main"] {
		t.Fatalf("config=%+v", got)
	}
}

func TestConfigResolvesMountedRootAndRejectsPathEscape(t *testing.T) {
	root := t.TempDir()
	config := Config{Mounts: map[string]string{"nas-main": root}}

	path, err := config.ResolveSourcePath("nas-main", "2026/clip.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "2026", "clip.mp4"); path != want {
		t.Fatalf("resolved path=%q want=%q", path, want)
	}

	for _, relative := range []string{"../outside.mp4", "/absolute.mp4", "2026/../../outside.mp4"} {
		if _, err := config.ResolveSourcePath("nas-main", relative); err == nil {
			t.Fatalf("expected path escape to be rejected: %q", relative)
		}
	}
}
