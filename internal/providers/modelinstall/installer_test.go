package modelinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testLock(t *testing.T, host, sha string, size int64) (*Lock, string) {
	t.Helper()
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "models.lock.json")
	content := `{
	  "schema_version": "inference.models.lock.v1",
	  "models": [{
	    "catalog_id": "visual-test",
	    "model_id": "test/model",
	    "revision": "rev-1",
	    "registry": {"type": "huggingface", "repo": "test/model", "hosts": ["` + host + `"]},
	    "files": [{"path": "weights.bin", "size": ` + itoa(size) + `, "sha256": "` + sha + `"}],
	    "license": {"code": "Apache-2.0", "weights": "Apache-2.0"}
	  }]
	}`
	if err := os.WriteFile(lockPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := LoadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	return lock, dir
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var digits []byte
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}

func TestLoadLockRejectsUnverifiableEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock.json")
	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"schema_version":"inference.models.lock.v1","models":[{"catalog_id":"a","files":[{"path":"../escape.bin","sha256":"x"}]}]}`)
	if _, err := LoadLock(path); err == nil {
		t.Fatal("a path escaping the model directory must be rejected")
	}
	write(`{"schema_version":"inference.models.lock.v1","models":[{"catalog_id":"a","files":[{"path":"weights.bin"}]}]}`)
	if _, err := LoadLock(path); err == nil {
		t.Fatal("a file without a hash must be rejected")
	}
	write(`{"schema_version":"inference.models.lock.v1","models":[{"catalog_id":"a","files":[{"path":"w","sha256":"x"}]},{"catalog_id":"a","files":[{"path":"w","sha256":"x"}]}]}`)
	if _, err := LoadLock(path); err == nil {
		t.Fatal("duplicate catalog ids must be rejected")
	}
}

func TestInstallVerifiesHashAndPublishesAtomically(t *testing.T) {
	payload := []byte("model weights")
	sum := sha256.Sum256(payload)
	sha := hex.EncodeToString(sum[:])
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/rev-1/weights.bin") {
			http.NotFound(w, r)
			return
		}
		w.Write(payload)
	}))
	defer server.Close()

	host := strings.Split(strings.TrimPrefix(server.URL, "https://"), ":")[0]
	lock, dir := testLock(t, host, sha, int64(len(payload)))
	installer := &Installer{
		Lock:     lock,
		ModelDir: filepath.Join(dir, "models"),
		Client:   server.Client(),
		URLFor: func(entry Entry, file File) (string, error) {
			return server.URL + "/test/model/rev-1/" + file.Path, nil
		},
	}
	digest, err := installer.Install(context.Background(), "visual-test")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("unexpected digest %q", digest)
	}
	published, err := os.ReadFile(filepath.Join(dir, "models", "visual-test", "weights.bin"))
	if err != nil {
		t.Fatalf("published file: %v", err)
	}
	if string(published) != string(payload) {
		t.Fatal("published bytes differ from the payload")
	}
	if err := installer.VerifyInstalled("visual-test"); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestInstallAbortsOnHashMismatchAndLeavesNothing(t *testing.T) {
	payload := []byte("tampered weights")
	wrong := sha256.Sum256([]byte("expected weights"))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer server.Close()
	host := strings.Split(strings.TrimPrefix(server.URL, "https://"), ":")[0]
	lock, dir := testLock(t, host, hex.EncodeToString(wrong[:]), int64(len(payload)))
	installer := &Installer{
		Lock:     lock,
		ModelDir: filepath.Join(dir, "models"),
		Client:   server.Client(),
		URLFor: func(entry Entry, file File) (string, error) {
			return server.URL + "/test/model/rev-1/" + file.Path, nil
		},
	}
	if _, err := installer.Install(context.Background(), "visual-test"); err == nil {
		t.Fatal("a hash mismatch must fail the install")
	}
	if _, err := os.Stat(filepath.Join(dir, "models", "visual-test", "weights.bin")); !os.IsNotExist(err) {
		t.Fatal("a failed install must not publish a file")
	}
	entries, err := os.ReadDir(filepath.Join(dir, "models"))
	if err == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".download-") {
				t.Fatalf("temporary file left behind: %s", entry.Name())
			}
		}
	}
}

func TestInstallRejectsURLOutsideTheLockedHosts(t *testing.T) {
	lock, dir := testLock(t, "huggingface.co", "deadbeef", 4)
	installer := &Installer{
		Lock:     lock,
		ModelDir: filepath.Join(dir, "models"),
		URLFor: func(entry Entry, file File) (string, error) {
			return "https://evil.example/weights.bin", nil
		},
	}
	if _, err := installer.Install(context.Background(), "visual-test"); err == nil {
		t.Fatal("a host outside the lock allowlist must be rejected")
	}
	if _, err := installer.Install(context.Background(), "unknown"); err == nil {
		t.Fatal("an unknown catalog id must be rejected")
	}
}

func TestInstallRejectsGatedModelWithoutAcceptance(t *testing.T) {
	lock, dir := testLock(t, "huggingface.co", "deadbeef", 4)
	lock.Models[0].ArtifactStatus = "blocked_gated"
	installer := &Installer{Lock: lock, ModelDir: filepath.Join(dir, "models")}
	if _, err := installer.Install(context.Background(), "visual-test"); err == nil {
		t.Fatal("a gated model must not install without access acceptance")
	}
}

func TestInstallRejectsOversizedResponse(t *testing.T) {
	payload := []byte("much longer than the locked size")
	sum := sha256.Sum256(payload)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer server.Close()
	host := strings.Split(strings.TrimPrefix(server.URL, "https://"), ":")[0]
	lock, dir := testLock(t, host, hex.EncodeToString(sum[:]), 4)
	installer := &Installer{
		Lock:     lock,
		ModelDir: filepath.Join(dir, "models"),
		Client:   server.Client(),
		URLFor: func(entry Entry, file File) (string, error) {
			return server.URL + "/test/model/rev-1/" + file.Path, nil
		},
	}
	if _, err := installer.Install(context.Background(), "visual-test"); err == nil {
		t.Fatal("a response larger than the locked size must be rejected")
	}
}
