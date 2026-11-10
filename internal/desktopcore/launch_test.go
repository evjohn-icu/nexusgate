package desktopcore

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fingerprint = "abababababababababababababababababababababababababababababababab"

func writeFakeCore(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-core")
	script := "#!/bin/sh\n" + body
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func fakeCoreReadyScript() string {
	return `
while [ $# -gt 0 ]; do
  case "$1" in
    --ready-file) READY="$2"; shift 2 ;;
    *) shift ;;
  esac
done
printf '{"url":"%s","fingerprint":"%s","pid":%s}' "$FAKE_URL" "$FAKE_FINGERPRINT" "${FAKE_PID:-$$}" > "$READY"
cat >/dev/null
`
}

func newTestHub(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestStartWaitsForMatchingReadyFileAndHealthyHub(t *testing.T) {
	hub := newTestHub(t)
	t.Setenv("FAKE_URL", hub.URL)
	t.Setenv("FAKE_FINGERPRINT", fingerprint)
	core := writeFakeCore(t, fakeCoreReadyScript())
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "admin-token"), []byte("local-admin-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	child, err := Start(context.Background(), Config{
		CoreBinary:   core,
		DataDir:      dataDir,
		ReadyTimeout: 5 * time.Second,
		StopTimeout:  2 * time.Second,
		HTTPClient:   hub.Client(),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if child.Endpoint.Token != "local-admin-token" {
		t.Fatalf("token=%q", child.Endpoint.Token)
	}
	if child.Endpoint.URL != hub.URL || child.Endpoint.PID == 0 {
		t.Fatalf("endpoint=%+v", child.Endpoint)
	}
	if err := child.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestStartRejectsForeignOrMismatchedReadyFile(t *testing.T) {
	hub := newTestHub(t)
	// PID mismatch: the ready file names a process that is not the child.
	t.Setenv("FAKE_URL", hub.URL)
	t.Setenv("FAKE_FINGERPRINT", fingerprint)
	t.Setenv("FAKE_PID", "1")
	core := writeFakeCore(t, fakeCoreReadyScript())
	dataDir := t.TempDir()
	os.WriteFile(filepath.Join(dataDir, "admin-token"), []byte("t"), 0o600)
	_, err := Start(context.Background(), Config{CoreBinary: core, DataDir: dataDir, ReadyTimeout: 5 * time.Second, HTTPClient: hub.Client()})
	if err == nil || !strings.Contains(err.Error(), "pid") {
		t.Fatalf("pid mismatch must fail, got %v", err)
	}

	// A plaintext or non-loopback URL is refused even if the child says it is
	// ready: the desktop never connects to a Hub it cannot pin on loopback.
	t.Setenv("FAKE_URL", "http://127.0.0.1:8787")
	t.Setenv("FAKE_PID", "0")
	core = writeFakeCore(t, fakeCoreReadyScript())
	if _, err := Start(context.Background(), Config{CoreBinary: core, DataDir: dataDir, ReadyTimeout: 5 * time.Second, HTTPClient: hub.Client()}); err == nil {
		t.Fatal("a plaintext ready URL must be refused")
	}
}

func TestStartTimesOutWithoutReadyFile(t *testing.T) {
	core := writeFakeCore(t, "sleep 30\n")
	_, err := Start(context.Background(), Config{
		CoreBinary:   core,
		DataDir:      t.TempDir(),
		ReadyTimeout: 150 * time.Millisecond,
		HTTPClient:   &http.Client{},
	})
	if err == nil || !strings.Contains(err.Error(), "ready") {
		t.Fatalf("expected a readiness timeout, got %v", err)
	}
}

func TestStopKillsAChildThatIgnoresStdinEOF(t *testing.T) {
	hub := newTestHub(t)
	t.Setenv("FAKE_URL", hub.URL)
	t.Setenv("FAKE_FINGERPRINT", fingerprint)
	script := `
while [ $# -gt 0 ]; do
  case "$1" in
    --ready-file) READY="$2"; shift 2 ;;
    *) shift ;;
  esac
done
printf '{"url":"%s","fingerprint":"%s","pid":%s}' "$FAKE_URL" "$FAKE_FINGERPRINT" "$$" > "$READY"
trap '' TERM
while true; do sleep 1; done
`
	core := writeFakeCore(t, script)
	dataDir := t.TempDir()
	os.WriteFile(filepath.Join(dataDir, "admin-token"), []byte("t"), 0o600)
	child, err := Start(context.Background(), Config{
		CoreBinary: core, DataDir: dataDir,
		ReadyTimeout: 5 * time.Second, StopTimeout: 150 * time.Millisecond,
		HTTPClient: hub.Client(),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	err = child.Stop()
	if err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("a child ignoring EOF must be killed and reported, got %v", err)
	}
}
