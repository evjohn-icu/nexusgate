package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/apiclient"
	"github.com/evjohn-icu/nexusgate/internal/credentials"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

func TestValidFingerprint(t *testing.T) {
	valid := strings.Repeat("ab", 32)
	if !ValidFingerprint(valid) {
		t.Fatal("valid SHA-256 fingerprints rejected")
	}
	if ValidFingerprint("") || ValidFingerprint("not-a-fingerprint") || ValidFingerprint(strings.Repeat("a", 64)+"0") || ValidFingerprint("ab:ab:ab:ab:ab:ab:ab:ab:ab:ab:ab:ab:ab:ab:ab:ab:ab:ab:ab:ab:ab:ab:ab:ab:ab:ab:ab:ab:ab:ab:ab:ab") {
		t.Fatal("invalid fingerprint accepted")
	}
}

func TestClientFingerprintMatchingAndMismatching(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"worker":{"id":"worker-1"},"token":"worker-token"}`))
	}))
	defer server.Close()
	sum := sha256.Sum256(server.Certificate().Raw)
	fingerprint := hex.EncodeToString(sum[:])
	registration := remote.WorkerRegistration{Name: "test", Platform: "linux-amd64"}
	if _, err := NewClient(server.URL, fingerprint).Enroll(context.Background(), "pairing", registration); err != nil {
		t.Fatalf("matching fingerprint rejected: %v", err)
	}
	if _, err := NewClient(server.URL, strings.Repeat("a", 64)).Enroll(context.Background(), "pairing", registration); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("mismatching fingerprint error = %v", err)
	}
}

func TestClientEnrollsThenSendsHeartbeat(t *testing.T) {
	var token, version string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/worker/enroll":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"worker": remote.Worker{ID: "worker-1"}, "token": "worker-token"})
		case "/api/v1/worker/heartbeat":
			token = r.Header.Get("Authorization")
			var body remote.WorkerHeartbeat
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			version = body.Version
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, "")
	registered, err := client.Enroll(context.Background(), "pairing", remote.WorkerRegistration{Name: "linux", Platform: "linux-arm64"})
	if err != nil || registered.Token != "worker-token" {
		t.Fatalf("registration=%+v err=%v", registered, err)
	}
	if err := client.Heartbeat(context.Background(), registered.Token, "v0.30.0", remote.WorkerCapabilities{Proxy: true}); err != nil {
		t.Fatal(err)
	}
	if token != "Bearer worker-token" {
		t.Fatalf("authorization=%q", token)
	}
	if version != "v0.30.0" {
		t.Fatalf("heartbeat version=%q, want v0.30.0", version)
	}
}

// The heartbeat wire body is the typed remote.WorkerHeartbeat, so a Hub that
// decodes it strictly accepts it: version next to capabilities, nothing else.
func TestClientHeartbeatSendsTypedBodyWithVersion(t *testing.T) {
	var rawBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/worker/heartbeat" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		rawBody = string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	err := NewClient(server.URL, "").Heartbeat(context.Background(), "worker-token", "v0.30.0", remote.WorkerCapabilities{Proxy: true, MaxParallelProxyJobs: 2})
	if err != nil {
		t.Fatal(err)
	}
	var decoded remote.WorkerHeartbeat
	if err := json.Unmarshal([]byte(rawBody), &decoded); err != nil {
		t.Fatalf("heartbeat body is not valid JSON: %q: %v", rawBody, err)
	}
	if decoded.Version != "v0.30.0" || !decoded.Capabilities.Proxy || decoded.Capabilities.MaxParallelProxyJobs != 2 {
		t.Fatalf("heartbeat body=%s", rawBody)
	}
}

func TestClientUploadsDerivedArtifactWithWorkerAuthAndMetadata(t *testing.T) {
	artifactPath := filepath.Join(t.TempDir(), "thumb.jpg")
	if err := os.WriteFile(artifactPath, []byte("thumbnail-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	var gotToken, gotType, gotProfile, gotFilename, gotContentType, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/worker/jobs/job-1/artifacts" {
			t.Fatalf("unexpected upload request: %s %s", r.Method, r.URL.Path)
		}
		gotToken = r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(1024 * 1024); err != nil {
			t.Fatal(err)
		}
		gotType = r.FormValue("type")
		gotProfile = r.FormValue("profile_hash")
		file, header, err := r.FormFile("artifact")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		gotFilename = header.Filename
		gotContentType = header.Header.Get("Content-Type")
		body, err := io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		gotBody = string(body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	client := NewClient(server.URL, "")
	err := client.UploadArtifact(context.Background(), "worker-token", "job-1", ArtifactUpload{Type: "thumbnail", ProfileHash: "thumb-software-v1", Path: artifactPath})
	if err != nil {
		t.Fatal(err)
	}
	if gotToken != "Bearer worker-token" || gotType != "thumbnail" || gotProfile != "thumb-software-v1" || gotFilename != "thumb.jpg" || gotContentType != "image/jpeg" || gotBody != "thumbnail-bytes" {
		t.Fatalf("upload metadata token=%q type=%q profile=%q filename=%q content_type=%q body=%q", gotToken, gotType, gotProfile, gotFilename, gotContentType, gotBody)
	}
}

func TestClientAcceptsIdempotentArtifactReuse(t *testing.T) {
	artifactPath := filepath.Join(t.TempDir(), "proxy.mp4")
	if err := os.WriteFile(artifactPath, []byte("proxy-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/worker/jobs/job-1/artifacts" {
			t.Fatalf("unexpected upload path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := NewClient(server.URL, "").UploadArtifact(context.Background(), "worker-token", "job-1", ArtifactUpload{Type: "proxy", ProfileHash: "proxy-v1", Path: artifactPath}); err != nil {
		t.Fatalf("idempotent upload should be accepted: %v", err)
	}
}

func TestClientRequestsTaskScopedCredentialWithoutWritingItToConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/worker/jobs/job-1/credentials/video_analysis" {
			t.Fatalf("unexpected credential request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer worker-token" {
			t.Fatalf("authorization=%q", got)
		}
		_, _ = w.Write([]byte(`{"job_id":"job-1","worker_id":"worker-1","provider":"volcengine_video","operation":"video_analysis","expires_at":"2026-07-26T00:05:00Z","credential":{"base_url":"https://vision.example","api_key":"memory-only","model":"vision-v1"}}`))
	}))
	defer server.Close()

	lease, err := NewClient(server.URL, "").Credential(context.Background(), "worker-token", "job-1", credentials.OperationVideoAnalysis)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Credential.APIKey != "memory-only" || lease.Provider != "volcengine_video" {
		t.Fatalf("unexpected credential lease: %#v", lease)
	}
}

func TestClientReportsProgressAndCanUseJSONOnlyProviderProxy(t *testing.T) {
	var progress map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/worker/jobs/job-1/progress":
			if got := r.Header.Get("Authorization"); got != "Bearer worker-token" {
				t.Fatalf("progress authorization=%q", got)
			}
			if err := json.NewDecoder(r.Body).Decode(&progress); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/worker/jobs/job-1/provider/video_analysis":
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("proxy content type=%q", got)
			}
			_, _ = w.Write([]byte(`{"summary":"Hub keeps the provider key"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewClient(server.URL, "")
	if err := client.Progress(context.Background(), "worker-token", "job-1", "derive", 42, "progress", "encoding"); err != nil {
		t.Fatal(err)
	}
	if progress["stage"] != "derive" || progress["progress"] != float64(42) {
		t.Fatalf("progress=%#v", progress)
	}
	result, err := client.ProxyProviderJSON(context.Background(), "worker-token", "job-1", credentials.OperationVideoAnalysis, json.RawMessage(`{"contents":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(result.Body) || string(result.Body) != `{"summary":"Hub keeps the provider key"}` {
		t.Fatalf("proxy result=%s", result.Body)
	}
}

func TestRedactSecretsStripsBearerAndOpenAIKeysFromErrorMessages(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "Bearer token in error",
			input: "provider returned HTTP 401: invalid Bearer sk-abc123def456ghi789jkl012mno345pqr678stu901vwx234",
			want:  "provider returned HTTP 401: invalid Bearer [redacted]",
		},
		{
			name:  "OpenAI key in error",
			input: "connect: API key sk-proj-abcdefghijklmnopqrstuvwxyz1234567890 is invalid",
			want:  "connect: API key [redacted] is invalid",
		},
		{
			name:  "X-Api-Key header value",
			input: "request failed: X-Api-Key: sk-secret-value-here-12345678",
			want:  "request failed: X-Api-Key: [redacted]",
		},
		{
			name:  "api_key in query-like text",
			input: `{"error":"api_key=sk-abcdefghijklmnopqrstuvwxyz123456 is malformed"}`,
			want:  `{"error":"api_key: [redacted] is malformed"}`,
		},
		{
			name:  "no secrets in text",
			input: "ffmpeg exited with code 1: file not found",
			want:  "ffmpeg exited with code 1: file not found",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactSecrets(tc.input)
			if got != tc.want {
				t.Fatalf("redactSecrets(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestRedactSecretsJSONKeyValues(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  `"api_key":"arbitrary-secret"`,
			input: `{"error":{"api_key":"arbitrary-secret-12345"}}`,
			want:  `{"error":{"api_key":"[redacted]"}}`,
		},
		{
			name:  `"authorization":"Bearer token"`,
			input: `{"authorization":"Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0"}`,
			want:  `{"authorization":"[redacted]"}`,
		},
		{
			name:  `"token":"short-secret"`,
			input: `{"token":"abc123"}`,
			want:  `{"token":"[redacted]"}`,
		},
		{
			name:  `"secret":"my-password-here"`,
			input: `{"secret":"my-password-here"}`,
			want:  `{"secret":"[redacted]"}`,
		},
		{
			name:  `"password":"p@ssw0rd!"`,
			input: `{"password":"p@ssw0rd!"}`,
			want:  `{"password":"[redacted]"}`,
		},
		{
			name:  `"apikey":"my-arbitrary-key"`,
			input: `{"apikey":"my-arbitrary-key"}`,
			want:  `{"apikey":"[redacted]"}`,
		},
		{
			name:  "unquoted JSON value after colon",
			input: `{"api_key":arbitrary-secret}`,
			want:  `{"api_key":[redacted]}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactSecrets(tc.input)
			if got != tc.want {
				t.Fatalf("redactSecrets(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestRedactSecretsNestedEscapedJSON(t *testing.T) {
	// P1: Escaped nested JSON — the outer {"body":"{\"token\":\"k\"}"}
	// parses as valid JSON, the inner string should be recursively parsed
	// and its "token" key redacted.  Prior to jsonAwareRedact, the regex
	// saw the escaped quotes and skipped the inner value entirely.
	input := `{"status":401,"body":"{\"api_key\":\"leaked-via-nesting\"}"}`
	got := redactSecrets(input)
	if strings.Contains(got, "leaked-via-nesting") {
		t.Fatalf("nested escaped JSON leaked secret: %s", got)
	}
	// The outer JSON should still be valid after redaction.
	if !json.Valid([]byte(got)) {
		t.Fatalf("redacted output is not valid JSON: %s", got)
	}
	// The body field should contain a redacted token.
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("nested JSON was not redacted: %s", got)
	}
}

func TestRedactSecretsNestedEscapedJSONShortToken(t *testing.T) {
	// P1: The specific bypass scenario — {"body":"{\"token\":\"k\"}"}
	// where "k" is too short for longTokenPattern.  The JSON-aware
	// redaction must catch "token":"k" inside the nested string.
	input := `{"body":"{\"token\":\"k\"}"}`
	got := redactSecrets(input)
	if strings.Contains(got, `"k"`) && strings.Contains(got, `"token"`) {
		t.Fatalf("short token in nested JSON not redacted: %s", got)
	}
	if !json.Valid([]byte(got)) {
		t.Fatalf("redacted output is not valid JSON: %s", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("nested JSON was not redacted: %s", got)
	}
}

func TestRedactSecretsJSONArrayOfObjects(t *testing.T) {
	// JSON arrays of objects should also be recursively redacted.
	input := `[{"api_key":"secret1"},{"api_key":"secret2"}]`
	got := redactSecrets(input)
	if strings.Contains(got, "secret1") || strings.Contains(got, "secret2") {
		t.Fatalf("JSON array leaked secrets: %s", got)
	}
	if !json.Valid([]byte(got)) {
		t.Fatalf("redacted array is not valid JSON: %s", got)
	}
}

func TestRedactSecretsJSONWithEmbeddedSegment(t *testing.T) {
	// Free-form text containing a JSON object segment should have the
	// segment redacted via jsonLikeSegment scanning.
	input := `upstream error: {"code":401,"api_key":"exposed-in-text"} extra context`
	got := redactSecrets(input)
	if strings.Contains(got, "exposed-in-text") {
		t.Fatalf("embedded JSON segment leaked secret: %s", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("embedded JSON segment was not redacted: %s", got)
	}
}

func TestRedactSecretsLongTokenFallback(t *testing.T) {
	// P2: longTokenPattern is now ≥20 chars starting with a letter.
	// 16-char hex strings, UUIDs, and timestamps are no longer redacted.
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "random 16-char hex token (too short, preserved)",
			input: "error: abcdef1234567890 is not valid",
			want:  "error: abcdef1234567890 is not valid",
		},
		{
			name:  "UUID with dashes (starts with digit, preserved)",
			input: "trace: 550e8400-e29b-41d4-a716-446655440000 request-id",
			want:  "trace: 550e8400-e29b-41d4-a716-446655440000 request-id",
		},
		{
			name:  "timestamp 16 digits (starts with digit, preserved)",
			input: "event at 20240101120000 failed",
			want:  "event at 20240101120000 failed",
		},
		{
			name:  "base64-like token 20+ chars starting with letter",
			input: "connect: abcdefghijklmnopqrst is bad",
			want:  "connect: [redacted] is bad",
		},
		{
			name:  "short token preserved (under 20)",
			input: "error: err-123 is ok",
			want:  "error: err-123 is ok",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactSecrets(tc.input)
			if got != tc.want {
				t.Fatalf("redactSecrets(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestProgressRedactsSecrets(t *testing.T) {
	var progress map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/worker/jobs/job-1/progress" {
			if err := json.NewDecoder(r.Body).Decode(&progress); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := NewClient(server.URL, "")
	err := client.Progress(context.Background(), "worker-token", "job-1",
		"analyze", 0.5, "provider error",
		`upstream returned: {"api_key":"sk-leaked-through-progress"}`)
	if err != nil {
		t.Fatal(err)
	}

	msg, _ := progress["message"].(string)
	if strings.Contains(msg, "sk-leaked-through-progress") {
		t.Fatalf("Progress.message leaked secret: %q", msg)
	}
	if !strings.Contains(msg, "[redacted]") {
		t.Fatalf("Progress.message was not redacted: %q", msg)
	}
}

// P1: Embedded JSON fragment inside a string field of a top-level JSON
// object — the string value is not itself valid JSON (it has a prefix),
// but contains a JSON fragment like {"token":"k"}. walkAndRedact must
// scan string fields for embedded JSON fragments.
func TestRedactSecretsEmbeddedFragmentInStringField(t *testing.T) {
	input := `{"body":"prefix {\"token\":\"k\"} suffix"}`
	got := redactSecrets(input)
	if strings.Contains(got, `"k"`) && strings.Contains(got, `"token"`) {
		t.Fatalf("embedded fragment in string field leaked secret: %s", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("embedded fragment was not redacted: %s", got)
	}
	if !json.Valid([]byte(got)) {
		t.Fatalf("redacted output is not valid JSON: %s", got)
	}
}

// P1: Deeply nested embedded JSON (beyond one level) should be found by
// bracket-balanced scanning and redacted.
func TestRedactSecretsDeeplyNestedEmbeddedJSON(t *testing.T) {
	input := `prefix {"access_token":"k","nested":{"deep":{"x":1}}} suffix`
	got := redactSecrets(input)
	if strings.Contains(got, `"k"`) && strings.Contains(got, `"access_token"`) {
		t.Fatalf("deeply nested embedded JSON leaked access_token: %s", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("deeply nested embedded JSON was not redacted: %s", got)
	}
}

// P1: After jsonAwareRedact processes an embedded segment, the result
// (e.g. {"token":"[redacted]"}) must not be corrupted by a subsequent
// jsonKeyPattern pass that strips quotes.
func TestRedactSecretsDoubleProcessingPreservesStructure(t *testing.T) {
	// This input's top level is not JSON, so it goes through embedded
	// segment scanning first, then regex fallbacks.
	input := `err: {"token":"secret-value"} end`
	got := redactSecrets(input)
	// After embedded-segment scan: err: {"token":"[redacted]"} end
	// jsonKeyPattern must NOT turn it into {"token":[redacted]}.
	if strings.Contains(got, `:[redacted]}`) && !strings.Contains(got, `:"[redacted]"}`) {
		t.Fatalf("double-processing stripped quotes from redacted value: %s", got)
	}
	if !strings.Contains(got, `"[redacted]"`) {
		t.Fatalf("embedded segment value was not redacted at all: %s", got)
	}
}

// P1: Long tokens adjacent to punctuation (token=value, token:"value")
// must be caught by credentialKeyValuePattern.
func TestRedactSecretsTokenAdjacentToPunctuation(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "token=equals form",
			input: "error: token=abcdefghijklmnopqrst",
		},
		{
			name:  "token:colon form",
			input: `error: token:"abcdefghijklmnopqrst"`,
		},
		{
			name:  "access_token= form",
			input: "error: access_token=abcdefghijklmnopqrst",
		},
		{
			name:  "secret= form",
			input: "error: secret=abcdefghijklmnopqrst",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactSecrets(tc.input)
			if !strings.Contains(got, "[redacted]") {
				t.Fatalf("token adjacent to punctuation not redacted: input=%q got=%q", tc.input, got)
			}
			if strings.Contains(got, "abcdefghijklmnopqrst") {
				t.Fatalf("token value not removed: got=%q", got)
			}
		})
	}
}

// P2: UUIDs starting with a letter (e50e8400-...) must not be redacted
// by the long-token safety net.
func TestRedactSecretsUUIDStartingWithLetterPreserved(t *testing.T) {
	input := "trace: e50e8400-e29b-41d4-a716-446655440000 request-id"
	got := redactSecrets(input)
	if got != input {
		t.Fatalf("UUID starting with letter was redacted: input=%q got=%q", input, got)
	}
}

// Top-level JSON string scalar that is itself an escaped JSON document must
// be walked (P3 from the fourth review round): the outer JSON is a bare
// string value, so without the string branch walkAndRedact would return it
// unchanged and the embedded short token would leak.
func TestRedactSecretsTopLevelJSONStringScalar(t *testing.T) {
	input := `"{\"token\":\"k\"}"`
	got := redactSecrets(input)
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("embedded token in top-level JSON string scalar not redacted: input=%q got=%q", input, got)
	}
	if strings.Contains(got, "\"k\"") {
		t.Fatalf("short token value still present: got=%q", got)
	}
}

// P1: credentialKeyValuePattern must match known keys case-insensitively.
func TestRedactSecretsCredentialKeyValueCaseInsensitive(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "ApiKey=value", input: "err: ApiKey=my-secret-key-here"},
		{name: "API_KEY=value", input: "err: API_KEY=sk-abcdefghijklmnopqrst"},
		{name: "apikey=value", input: "err: apikey=my-secret-key-here"},
		{name: "api_key_id=value", input: "err: api_key_id=abcdefghijklmnopqrst"},
		{name: "credential=value", input: "err: credential=abcdefghijklmnopqrst"},
		{name: "authorization=value", input: "err: authorization=abcdefghijklmnopqrst"},
		{name: "key=value", input: "err: key=abcdefghijklmnopqrst"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactSecrets(tc.input)
			if !strings.Contains(got, "[redacted]") {
				t.Fatalf("credential key=value not redacted (case-insensitive): input=%q got=%q", tc.input, got)
			}
		})
	}
}

func TestUploadArtifactCancelsOnContextDone(t *testing.T) {
	artifactPath := filepath.Join(t.TempDir(), "large.bin")
	if err := os.WriteFile(artifactPath, make([]byte, 10<<20), 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := NewClient(server.URL, "")
	err := client.UploadArtifact(ctx, "worker-token", "job-1", ArtifactUpload{
		Type:        "proxy",
		ProfileHash: "proxy-v1",
		Path:        artifactPath,
	})
	if err == nil {
		t.Fatal("expected context cancellation to propagate to UploadArtifact")
	}
	if !strings.Contains(err.Error(), "cancel") && !strings.Contains(err.Error(), "closed") {
		t.Fatalf("error should reflect cancellation, got: %v", err)
	}
}

// TestWorkerClientHubErrorEnvelope verifies that Lease and postJSON decode the
// Hub's JSON error envelope into *apiclient.Error and surface the stable Code
// (without echoing an unbounded raw body) so the Worker can classify failures
// instead of parsing prose.
func TestWorkerClientHubErrorEnvelope(t *testing.T) {
	// Lease: a Hub envelope error must decode and surface the code.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"job_lease_lost","message":"job lease no longer held","retryable":true,"action":"reacquire_lease"}}`))
	}))
	defer server.Close()
	_, err := NewClient(server.URL, "").Lease(context.Background(), "worker-token")
	if err == nil {
		t.Fatal("expected a lease error")
	}
	var apiErr *apiclient.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("lease error = %T, want *apiclient.Error", err)
	}
	if apiErr.StatusCode != http.StatusConflict || apiErr.Code != "job_lease_lost" || !apiErr.Retryable || apiErr.Action != "reacquire_lease" {
		t.Fatalf("lease envelope = %+v", apiErr)
	}
	if !strings.Contains(apiErr.Error(), "job_lease_lost") {
		t.Fatalf("lease error must surface the code, got: %v", apiErr)
	}
	if strings.Contains(apiErr.Error(), `{"error":{"code"`) {
		t.Fatalf("lease error must not echo the raw envelope body, got: %v", apiErr)
	}

	// postJSON (Heartbeat): the same envelope decoding applies.
	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"admin_auth_required","message":"a valid administrator token is required","retryable":false}}`))
	}))
	defer server2.Close()
	err = NewClient(server2.URL, "").Heartbeat(context.Background(), "bad-token", "v1", remote.WorkerCapabilities{})
	if err == nil {
		t.Fatal("expected a heartbeat error")
	}
	if !errors.As(err, &apiErr) {
		t.Fatalf("heartbeat error = %T, want *apiclient.Error", err)
	}
	if apiErr.Code != "admin_auth_required" || apiErr.Retryable {
		t.Fatalf("heartbeat envelope = %+v", apiErr)
	}
	if !strings.Contains(apiErr.Error(), "admin_auth_required") {
		t.Fatalf("heartbeat error must surface the code, got: %v", apiErr)
	}

	// A non-envelope worker error still decodes to a bounded message.
	server3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer server3.Close()
	_, err = NewClient(server3.URL, "").Lease(context.Background(), "worker-token")
	if err == nil {
		t.Fatal("expected a lease error")
	}
	if !errors.As(err, &apiErr) {
		t.Fatalf("lease error = %T, want *apiclient.Error", err)
	}
	if apiErr.Code != "" || !strings.Contains(apiErr.Error(), "boom") {
		t.Fatalf("non-envelope lease error = %+v (%q)", apiErr, apiErr.Error())
	}
}
