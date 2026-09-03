package worker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/remote"
)

func TestConfigRoundTripKeepsNodeTokenButNotProviderSecrets(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "worker.json")
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

func TestConfigSaveIsAtomicAndFailClosed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "worker.json")
	first := Config{HubURL: "https://one", Token: "token-one"}
	second := Config{HubURL: "https://two", Token: "token-two"}
	if err := SaveConfig(path, first); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(path, second); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.HubURL != second.HubURL || got.Token != second.Token {
		t.Fatalf("got=%+v", got)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("mode/type=%o/%v", info.Mode().Perm(), info.Mode().IsRegular())
	}

	target := filepath.Join(t.TempDir(), "target.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(path, second); err == nil {
		t.Fatal("expected save symlink rejection")
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected load symlink rejection")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected non-regular rejection")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"hub_url":"https://x","token":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected insecure permission rejection")
	}
}

func TestConfigSaveRejectsInsecureExistingFileAndParent(t *testing.T) {
	config := Config{HubURL: "https://worker", Token: "node-token"}
	dir := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "worker.json")
	original := []byte(`{"hub_url":"https://original","token":"original"}`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(path, config); err == nil {
		t.Fatal("expected insecure existing file rejection")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("rejected save changed file: got %q want %q", got, original)
	}

	insecureDir := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(insecureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(filepath.Join(insecureDir, "worker.json"), config); err == nil {
		t.Fatal("expected insecure parent rejection")
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
