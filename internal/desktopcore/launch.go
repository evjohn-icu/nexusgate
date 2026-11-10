// Package desktopcore launches and supervises the bundled Hub core process for
// the native desktop client.
//
// It contains no UI and no Wails dependency, so the lifecycle can be tested
// without a WebView. The contract with the core binary is intentionally small:
// bind a loopback port, write a ready file with {url,fingerprint,pid}, and shut
// down when stdin closes. A parent crash closes the pipe, which is the only
// reliable "stop" signal a child cannot ignore.
package desktopcore

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ReadyPayload is the child's handshake.
type ReadyPayload struct {
	URL         string `json:"url"`
	Fingerprint string `json:"fingerprint"`
	PID         int    `json:"pid"`
}

// Config pins the child binary and its private data directory.
type Config struct {
	// CoreBinary is the sibling `nexusgate` binary. It is never taken from a
	// URL or a user-supplied path in the UI.
	CoreBinary string
	// DataDir is the desktop-private application data directory. The admin
	// token the child generates lives here and is read by Go only.
	DataDir string
	// ReadyTimeout bounds startup. A model-free Hub binds in milliseconds; a
	// slow disk or a first migration can take a few seconds.
	ReadyTimeout time.Duration
	// StopTimeout bounds the graceful shutdown before the process is killed.
	StopTimeout time.Duration
	// HTTPClient verifies health. Nil builds a client that pins the
	// fingerprint from the ready file.
	HTTPClient *http.Client
}

// Child is one running core process.
type Child struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	Endpoint    Endpoint
	done        chan error
	stopTimeout time.Duration
}

// Endpoint is what the UI needs to connect.
type Endpoint struct {
	URL         string
	Fingerprint string
	PID         int
	Token       string
}

// Start launches the core and waits for a verified, healthy ready file.
func Start(ctx context.Context, cfg Config) (*Child, error) {
	if strings.TrimSpace(cfg.CoreBinary) == "" || strings.TrimSpace(cfg.DataDir) == "" {
		return nil, fmt.Errorf("desktop core: binary and data dir are required")
	}
	info, err := os.Stat(cfg.CoreBinary)
	if err != nil || info.IsDir() {
		return nil, fmt.Errorf("desktop core: binary is unavailable")
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = 20 * time.Second
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = 10 * time.Second
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	readyDir := filepath.Join(cfg.DataDir, "desktop")
	if err := os.MkdirAll(readyDir, 0o700); err != nil {
		return nil, err
	}
	readyPath := filepath.Join(readyDir, "ready.json")
	_ = os.Remove(readyPath)

	cmd := exec.Command(cfg.CoreBinary,
		"serve", "-addr", "127.0.0.1:0",
		"--ready-file", readyPath,
		"--shutdown-on-stdin-eof",
	)
	cmd.Env = append(os.Environ(),
		"NEXUSGATE_DATA_DIR="+cfg.DataDir,
		"NEXUSGATE_HUB_ADMIN_AUTH=required",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	configureProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("desktop core: start: %w", err)
	}
	child := &Child{cmd: cmd, stdin: stdin, done: make(chan error, 1), stopTimeout: cfg.StopTimeout}
	go func() { child.done <- cmd.Wait() }()

	payload, err := waitForReady(ctx, readyPath, cfg.ReadyTimeout, child.done)
	if err != nil {
		child.kill()
		return nil, err
	}
	if payload.PID != cmd.Process.Pid {
		child.kill()
		return nil, fmt.Errorf("desktop core: ready file names pid %d, but the child is %d", payload.PID, cmd.Process.Pid)
	}
	parsed, err := url.Parse(payload.URL)
	if err != nil || parsed.Scheme != "https" || !isLoopbackHost(parsed.Hostname()) {
		child.kill()
		return nil, fmt.Errorf("desktop core: ready URL is not a loopback HTTPS endpoint")
	}
	if len(payload.Fingerprint) != sha256.Size*2 {
		child.kill()
		return nil, fmt.Errorf("desktop core: ready fingerprint is malformed")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = pinnedClient(payload.Fingerprint)
	}
	if err := waitHealthy(ctx, client, payload.URL, cfg.ReadyTimeout); err != nil {
		child.kill()
		return nil, err
	}
	token, err := readAdminToken(cfg.DataDir)
	if err != nil {
		child.kill()
		return nil, err
	}
	child.Endpoint = Endpoint{
		URL:         payload.URL,
		Fingerprint: payload.Fingerprint,
		PID:         payload.PID,
		Token:       token,
	}
	return child, nil
}

// Stop closes stdin, waits for a graceful exit, then kills the process tree.
func (c *Child) Stop() error {
	if c == nil || c.cmd == nil {
		return nil
	}
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	timeout := time.NewTimer(c.stopTimeout)
	defer timeout.Stop()
	select {
	case err := <-c.done:
		if err != nil && !isExitSuccess(err) {
			return err
		}
		return nil
	case <-timeout.C:
		c.kill()
		<-c.done
		return fmt.Errorf("desktop core did not stop within its timeout; killed")
	}
}

// kill terminates the whole process group. A bare Process.Kill would leave a
// pipeline worker or a Python child behind.
func (c *Child) kill() {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return
	}
	killProcessGroup(c.cmd.Process)
}

func waitForReady(ctx context.Context, path string, timeout time.Duration, done <-chan error) (ReadyPayload, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ReadyPayload{}, ctx.Err()
		case err := <-done:
			return ReadyPayload{}, fmt.Errorf("desktop core exited before readiness: %v", err)
		case <-deadline.C:
			return ReadyPayload{}, fmt.Errorf("desktop core did not become ready within %s", timeout)
		case <-ticker.C:
			raw, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var payload ReadyPayload
			if err := json.Unmarshal(raw, &payload); err != nil {
				continue
			}
			if payload.URL == "" || payload.PID == 0 {
				continue
			}
			return payload, nil
		}
	}
}

func waitHealthy(ctx context.Context, client *http.Client, baseURL string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("desktop core never answered a pinned health check")
		case <-ticker.C:
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/v1/health", nil)
			if err != nil {
				return err
			}
			response, err := client.Do(request)
			if err != nil {
				continue
			}
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
}

func readAdminToken(dataDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, "admin-token"))
	if err != nil {
		return "", fmt.Errorf("desktop core: admin token is unavailable: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("desktop core: admin token is empty")
	}
	return token, nil
}

func pinnedClient(fingerprint string) *http.Client {
	expected := strings.ToLower(strings.TrimSpace(fingerprint))
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("desktop core presented no certificate")
			}
			sum := sha256.Sum256(rawCerts[0])
			if hex.EncodeToString(sum[:]) != expected {
				return fmt.Errorf("desktop core certificate fingerprint mismatch")
			}
			return nil
		},
	}
	return &http.Client{Transport: transport, Timeout: 15 * time.Second}
}

func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	parsed := net.ParseIP(host)
	return parsed != nil && parsed.IsLoopback()
}

func isExitSuccess(err error) bool {
	if err == nil {
		return true
	}
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.Success()
}
