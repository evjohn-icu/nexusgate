package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/app"
	"github.com/evjohn-icu/nexusslate/internal/config"
	"github.com/evjohn-icu/nexusslate/internal/hubtls"
	"github.com/evjohn-icu/nexusslate/internal/media"
	"github.com/evjohn-icu/nexusslate/internal/remote"
	"github.com/evjohn-icu/nexusslate/internal/repository/sqlite"
)

// TestWorkerBootstrapBinaryAuth is the pairing-token guard matrix for the
// Worker binary download. The route admits a trusted source address, a Hub
// admin/agent token, or a valid, unredeemed X-NexusSlate-Pairing-Token header —
// and nothing else. A valid token authorizes repeated downloads without being
// consumed, while an expired, redeemed, or absent token is rejected; after
// enrollment redeems the token, the same credential can no longer download.
func TestWorkerBootstrapBinaryAuth(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "bootstrap-auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dataDir := secureTestDataDir(t)
	binDir := filepath.Join(dataDir, "worker-binaries")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binaryContent := []byte("bootstrap-binary-content")
	if err := os.WriteFile(filepath.Join(binDir, "nexusslate-linux-amd64"), binaryContent, 0o755); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: dataDir, Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	validPairing, err := repo.CreateWorkerPairing(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	expiredPairing, err := repo.CreateWorkerPairing(ctx, time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	redeemedPairing, err := repo.CreateWorkerPairing(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.EnrollWorker(ctx, redeemedPairing.Token, remote.WorkerRegistration{Name: "redeemer", Platform: "linux-amd64"}); err != nil {
		t.Fatal(err)
	}

	remoteRequest := func(header string) *http.Request {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/hub/worker-binaries/linux-amd64", nil)
		request.RemoteAddr = "203.0.113.50:9999"
		if header != "" {
			request.Header.Set("X-NexusSlate-Pairing-Token", header)
		}
		return request
	}

	t.Run("missing token rejected", func(t *testing.T) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, remoteRequest(""))
		if response.Code != http.StatusForbidden {
			t.Fatalf("status=%d body=%s, want 403", response.Code, response.Body.String())
		}
	})
	t.Run("expired token rejected", func(t *testing.T) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, remoteRequest(expiredPairing.Token))
		if response.Code != http.StatusForbidden {
			t.Fatalf("status=%d body=%s, want 403", response.Code, response.Body.String())
		}
	})
	t.Run("redeemed token rejected", func(t *testing.T) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, remoteRequest(redeemedPairing.Token))
		if response.Code != http.StatusForbidden {
			t.Fatalf("status=%d body=%s, want 403", response.Code, response.Body.String())
		}
	})
	t.Run("valid token admits without consuming", func(t *testing.T) {
		for attempt := range 2 {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, remoteRequest(validPairing.Token))
			if response.Code != http.StatusOK {
				t.Fatalf("attempt %d status=%d body=%s, want 200", attempt, response.Code, response.Body.String())
			}
			if !bytes.Equal(response.Body.Bytes(), binaryContent) {
				t.Fatalf("attempt %d returned the wrong binary", attempt)
			}
		}
	})
	t.Run("token still valid after downloads then rejected after enrollment", func(t *testing.T) {
		if _, _, err := repo.EnrollWorker(ctx, validPairing.Token, remote.WorkerRegistration{Name: "consumer", Platform: "linux-amd64"}); err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, remoteRequest(validPairing.Token))
		if response.Code != http.StatusForbidden {
			t.Fatalf("status=%d body=%s, want 403 after enrollment redeemed it", response.Code, response.Body.String())
		}
	})
	t.Run("trusted network admitted without a token", func(t *testing.T) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/hub/worker-binaries/linux-amd64", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s, want 200", response.Code, response.Body.String())
		}
	})
	t.Run("admin and agent tokens admitted remotely", func(t *testing.T) {
		for _, request := range []*http.Request{
			hubAdminRequest(service, http.MethodGet, "/api/v1/hub/worker-binaries/linux-amd64", nil),
			hubAgentRequest(service, http.MethodGet, "/api/v1/hub/worker-binaries/linux-amd64", nil),
		} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s, want 200", response.Code, response.Body.String())
			}
		}
	})
	t.Run("unknown platform still 404", func(t *testing.T) {
		freshPairing, err := repo.CreateWorkerPairing(ctx, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, remoteRequestWithToken("/api/v1/hub/worker-binaries/darwin-arm64", freshPairing.Token))
		if response.Code != http.StatusNotFound {
			t.Fatalf("status=%d body=%s, want 404", response.Code, response.Body.String())
		}
	})
}

func remoteRequestWithToken(target, token string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.RemoteAddr = "203.0.113.50:9999"
	request.Header.Set("X-NexusSlate-Pairing-Token", token)
	return request
}

// TestGeneratedWorkerScriptPinsAndVerifiesBinary asserts the bootstrap trust
// chain the generated scripts embed: the certificate fingerprint and SPKI pin
// match the Hub's actual TLS identity, the binary digest matches the artifact,
// the pairing token travels in a request header, the config lands in a private
// 0700 directory, and the download is verified before it is renamed into place.
func TestGeneratedWorkerScriptPinsAndVerifiesBinary(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "script-pins.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dataDir := secureTestDataDir(t)
	binaryContent := []byte("the-worker-binary-payload")
	service := mustScriptService(t, repo, dataDir)
	handler, certPath := workerSetupTLSServerWithCert(t, service, map[string]string{"linux-amd64": string(binaryContent), "windows-amd64": string(binaryContent)})

	fingerprint, err := hubtls.FingerprintCertificate(certPath)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := hubtls.LoadCertificate(certPath)
	if err != nil {
		t.Fatal(err)
	}
	spkiPin := hubtls.CertificateSPKIPin(leaf)
	binarySum := sha256.Sum256(binaryContent)
	binaryDigest := hex.EncodeToString(binarySum[:])

	for _, platform := range []string{"linux-amd64", "windows-amd64"} {
		t.Run(platform, func(t *testing.T) {
			response := httptest.NewRecorder()
			request := tlsAdminRequest(service, http.MethodPost, "/api/v1/hub/worker-setup/script", strings.NewReader(`{"platform":"`+platform+`","name":"pin-worker","pairing_token":"tok-pin"}`))
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			script := response.Body.String()

			// Every identity value the script carries must match the Hub's real
			// TLS identity and the artifact digest, never a stale copy. POSIX
			// pins the download with the SPKI pin and the Worker with the
			// fingerprint; PowerShell pins the download with the fingerprint
			// itself, so no SPKI pin is embedded there.
			if platform == "linux-amd64" {
				if !strings.Contains(script, "FINGERPRINT='"+fingerprint+"'") {
					t.Fatalf("script fingerprint mismatch:\n%s", script)
				}
				if !strings.Contains(script, "SPKI_PIN='"+spkiPin+"'") {
					t.Fatalf("script SPKI pin mismatch:\n%s", script)
				}
				if !strings.Contains(script, "BINARY_DIGEST='"+binaryDigest+"'") {
					t.Fatalf("script binary digest mismatch:\n%s", script)
				}
			} else {
				if !strings.Contains(script, "$fingerprint = '"+fingerprint+"'") {
					t.Fatalf("PowerShell fingerprint mismatch:\n%s", script)
				}
				if !strings.Contains(script, "$binaryDigest = '"+binaryDigest+"'") {
					t.Fatalf("PowerShell digest mismatch:\n%s", script)
				}
			}

			// --fingerprint must precede --pairing, and the config path is the
			// private .nexusslate-worker/worker.json, not the old top-level file.
			if strings.Index(script, "--fingerprint") > strings.Index(script, "--pairing") {
				t.Fatalf("--fingerprint must appear before --pairing:\n%s", script)
			}
			if !strings.Contains(script, "--config ./.nexusslate-worker/worker.json") {
				t.Fatalf("script does not use the private config path:\n%s", script)
			}
			if !strings.Contains(script, "worker run --config ./.nexusslate-worker/worker.json") || !strings.Contains(script, "worker doctor --config ./.nexusslate-worker/worker.json") {
				t.Fatalf("run/doctor instructions must use the private config path:\n%s", script)
			}
			if strings.Contains(script, "nexusslate-worker.json") {
				t.Fatalf("script still references the legacy top-level config path:\n%s", script)
			}
			if !strings.Contains(script, "X-NexusSlate-Pairing-Token") {
				t.Fatalf("script must carry the pairing token in a request header:\n%s", script)
			}

			if platform == "linux-amd64" {
				for _, marker := range []string{
					"umask 077",
					"mkdir -p \"$CONFIG_DIR\"",
					"chmod 0700 \"$CONFIG_DIR\"",
					"--pinnedpubkey",
					"sha256sum -c -",
					"rm -f \"$BINARY_TMP\"",
					"mv \"$BINARY_TMP\" \"$BINARY_OUT\"",
					"chmod 0755 \"$BINARY_OUT\"",
					"nexusslate-linux-amd64.download",
				} {
					if !strings.Contains(script, marker) {
						t.Fatalf("POSIX script missing %q:\n%s", marker, script)
					}
				}
			} else {
				for _, marker := range []string{
					"New-Item -ItemType Directory -Force",
					"ServerCertificateCustomValidationCallback",
					"$certificate.RawData",
					"Get-FileHash",
					"Move-Item -Force",
					"Remove-Item -Force -Path $binaryTmp",
					"nexusslate-windows-amd64.exe.download",
				} {
					if !strings.Contains(script, marker) {
						t.Fatalf("PowerShell script missing %q:\n%s", marker, script)
					}
				}
			}
		})
	}
}

func mustScriptService(t *testing.T, repo *sqlite.Repository, dataDir string) *app.Service {
	t.Helper()
	service, err := app.NewService(repo, config.Config{DataDir: dataDir, Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// workerSetupTLSServerWithCert is workerSetupTLSServer plus the certificate
// path, so a test can assert the script's embedded identity matches the Hub's.
func workerSetupTLSServerWithCert(t *testing.T, service *app.Service, binaries map[string]string) (http.Handler, string) {
	t.Helper()
	dataDir := service.DataDir()
	binDir := filepath.Join(dataDir, "worker-binaries")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for platform, content := range binaries {
		info := workerPlatforms[platform]
		if err := os.WriteFile(filepath.Join(binDir, info.Filename), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	certFile, keyFile, _, err := hubtls.EnsureSelfSigned(filepath.Join(dataDir, "tls"))
	if err != nil {
		t.Fatal(err)
	}
	return NewTLSServer("", service, certFile, keyFile).Handler(), certFile
}

// TestWorkerSetupScriptGenerationRequiresBinaryAndTLS pins the 503 responses
// generation answers when the bootstrap preconditions are missing: no artifact
// and no HTTPS identity mean no script, so a pairing token is never minted
// fruitlessly.
func TestWorkerSetupScriptGenerationRequiresBinaryAndTLS(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "script-required.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dataDir := secureTestDataDir(t)
	service, err := app.NewService(repo, config.Config{DataDir: dataDir, Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("no binary and no TLS", func(t *testing.T) {
		handler := NewServer("", service).Handler()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/hub/worker-setup/script", strings.NewReader(`{"platform":"linux-amd64","pairing_token":"tok"}`)))
		assertEnvelopeError(t, response, http.StatusServiceUnavailable, "worker_binary_unavailable", "upload_worker_binary")
	})

	t.Run("TLS but no binary", func(t *testing.T) {
		handler := workerSetupTLSServer(t, service, map[string]string{})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, tlsAdminRequest(service, http.MethodPost, "/api/v1/hub/worker-setup/script", strings.NewReader(`{"platform":"linux-amd64","pairing_token":"tok"}`)))
		assertEnvelopeError(t, response, http.StatusServiceUnavailable, "worker_binary_unavailable", "upload_worker_binary")
	})

	t.Run("binary but no TLS", func(t *testing.T) {
		binDir := filepath.Join(dataDir, "worker-binaries")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(binDir, "nexusslate-linux-amd64"), []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
		handler := NewServer("", service).Handler()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/hub/worker-setup/script", strings.NewReader(`{"platform":"linux-amd64","pairing_token":"tok"}`)))
		assertEnvelopeError(t, response, http.StatusServiceUnavailable, "worker_tls_required", "enable_hub_tls")
	})
}

func assertEnvelopeError(t *testing.T, response *httptest.ResponseRecorder, wantStatus int, wantCode, wantAction string) {
	t.Helper()
	if response.Code != wantStatus {
		t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), wantStatus)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"code":"`+wantCode+`"`) {
		t.Fatalf("error body missing code %q: %s", wantCode, body)
	}
	if wantAction != "" && !strings.Contains(body, `"action":"`+wantAction+`"`) {
		t.Fatalf("error body missing action %q: %s", wantAction, body)
	}
}

// TestWorkerSetupRetriesContextAfterLogin pins the post-login recovery wiring:
// the context loader is one idempotent function that reloads BOTH the setup
// context and the admin library-root details when a login succeeds, so an
// off-trusted-network administrator is not stuck with the pre-login redacted
// view. The redacted-root fallback and the generation guard must remain.
func TestWorkerSetupRetriesContextAfterLogin(t *testing.T) {
	body := workerSetupPageHTML

	// The loader is a named, idempotent function invoked both at load time and
	// after a successful login event.
	if !strings.Contains(body, "async function loadContext()") {
		t.Fatal("page must define an idempotent loadContext() loader")
	}
	if !strings.Contains(body, "function init(){return loadContext()}") {
		t.Fatal("page must route its initial load through loadContext()")
	}
	authChanged := `window.addEventListener('nexusslate:admin-auth-changed',function(event){if(event.detail&&event.detail.authenticated){loadContext()}else{ctx=ctx||{};ctx.library_roots=(ctx.library_roots||[]).map(function(root){return{id:root.id}});renderMounts()}});`
	if !strings.Contains(body, authChanged) {
		t.Fatal("a successful login must retry loadContext(); a logout must keep the redacted-root fallback")
	}

	// The context response must carry the SPKI pin so the page can decide
	// whether generation is possible without minting a pairing token first.
	if !strings.Contains(body, "ctx.spki_pin") {
		t.Fatal("page must check ctx.spki_pin before generating")
	}
	if !strings.Contains(body, "tdT('workerSetup.binaryRequired')") || !strings.Contains(body, "tdT('workerSetup.tlsRequired')") {
		t.Fatal("generation guard must explain missing binaries and missing TLS")
	}

	// The pairing token must only be minted after the guard passes: the token
	// POST must appear after the availability checks in generateScript().
	script := workerSetupPageHTML[strings.Index(body, "async function generateScript()"):]
	pairingPos := strings.Index(script, "worker-pairings")
	binaryGuardPos := strings.Index(script, "binaryRequired")
	tlsGuardPos := strings.Index(script, "tlsRequired")
	if pairingPos < 0 || binaryGuardPos < 0 || tlsGuardPos < 0 {
		t.Fatalf("generateScript is missing the guard or the pairing mint:\n%s", script)
	}
	if pairingPos < binaryGuardPos || pairingPos < tlsGuardPos {
		t.Fatal("generateScript mints the pairing token before the availability guard")
	}
}

// TestWorkerSetupContextExposesSPKIPin checks the context JSON actually carries
// the pin a real TLS Hub derives, so the page-level generation guard has data
// to evaluate rather than a hardcoded assumption.
func TestWorkerSetupContextExposesSPKIPin(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "ctx-spki.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dataDir := secureTestDataDir(t)
	service, err := app.NewService(repo, config.Config{DataDir: dataDir, Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile, _, err := hubtls.EnsureSelfSigned(filepath.Join(dataDir, "tls"))
	if err != nil {
		t.Fatal(err)
	}
	handler := NewTLSServer("", service, certFile, keyFile).Handler()

	response := httptest.NewRecorder()
	request := hubAdminRequest(service, http.MethodGet, "/api/v1/hub/worker-setup/context", nil)
	request.TLS = &tls.ConnectionState{}
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	rawBody := response.Body.String()
	var data struct {
		Fingerprint string `json:"fingerprint"`
		SPKIPin     string `json:"spki_pin"`
		TLS         bool   `json:"tls"`
	}
	if err := json.NewDecoder(strings.NewReader(rawBody)).Decode(&data); err != nil {
		t.Fatal(err)
	}
	if !data.TLS {
		t.Fatal("expected tls=true with a TLS certificate configured")
	}
	if data.Fingerprint == "" {
		t.Fatal("context must expose the certificate fingerprint")
	}
	if !regexp.MustCompile(`^sha256//[A-Za-z0-9+/=]+$`).MatchString(data.SPKIPin) {
		t.Fatalf("context must expose a curl-compatible sha256// SPKI pin, got %q", data.SPKIPin)
	}
	if !strings.Contains(rawBody, `"spki_pin"`) {
		t.Fatal("context JSON is missing the spki_pin field")
	}
}

// TestGeneratedScriptEnrollsCompiledBinaryAgainstLocalTLSHub is the HTTPS
// smoke for this bootstrap flow: it builds the real nexusslate binary, serves
// a real Hub handler over a self-signed TLS fixture, and runs
// `worker enroll` with exactly the arguments the generated install scripts
// produce (--hub, --fingerprint before --pairing, --config in a private
// .nexusslate-worker directory). The pinned fingerprint must authenticate the
// fixture, the config must persist with that same fingerprint, and the pairing
// token must be redeemed exactly once.
func TestGeneratedScriptEnrollsCompiledBinaryAgainstLocalTLSHub(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain unavailable to build the nexusslate binary")
	}
	workDir := t.TempDir()
	binPath := filepath.Join(workDir, "nexusslate-smoke")
	build := exec.Command(goTool, "build", "-o", binPath, "github.com/evjohn-icu/nexusslate/cmd/nexusslate")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building nexusslate failed: %v\n%s", err, out)
	}

	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(workDir, "smoke.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: filepath.Join(workDir, "data"), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	pairing, err := repo.CreateWorkerPairing(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// Serve the real Hub handler over a self-signed TLS identity exactly like
	// the one hubtls.EnsureSelfSigned provisions for production.
	certFile, keyFile, _, err := hubtls.EnsureSelfSigned(filepath.Join(workDir, "tls"))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := hubtls.FingerprintCertificate(certFile)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(NewServer("", service).Handler())
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	server.StartTLS()
	defer server.Close()

	configPath := filepath.Join(workDir, ".nexusslate-worker", "worker.json")
	enroll := exec.Command(binPath, "worker", "enroll",
		"--hub", server.URL,
		"--fingerprint", fingerprint,
		"--pairing", pairing.Token,
		"--config", configPath,
	)
	enroll.Env = append(os.Environ(), "NEXUSSLATE_WORKER_CONFIG="+configPath)
	if out, err := enroll.CombinedOutput(); err != nil {
		t.Fatalf("worker enroll failed: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("worker.json not written by enroll: %v", err)
	}
	var config struct {
		HubURL                 string `json:"hub_url"`
		CertificateFingerprint string `json:"certificate_fingerprint"`
		Token                  string `json:"token"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	if config.HubURL != server.URL || config.CertificateFingerprint != fingerprint || config.Token == "" {
		t.Fatalf("worker.json does not carry the pinned identity: %+v", config)
	}

	// Enrollment redeemed the token exactly once; the same credential must not
	// authorize a second worker.
	if valid, err := repo.WorkerPairingValid(ctx, pairing.Token, time.Now().UTC()); err != nil || valid {
		t.Fatalf("pairing token still valid after enrollment (valid=%v err=%v), want redeemed", valid, err)
	}
	second := exec.Command(binPath, "worker", "enroll",
		"--hub", server.URL,
		"--fingerprint", fingerprint,
		"--pairing", pairing.Token,
		"--config", filepath.Join(workDir, ".nexusslate-worker", "second.json"),
	)
	if out, err := second.CombinedOutput(); err == nil {
		t.Fatalf("reusing a redeemed pairing token should fail, got: %s", out)
	}
}
