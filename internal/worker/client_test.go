package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ev/timingdex/internal/credentials"
	"github.com/ev/timingdex/internal/remote"
)

func TestClientEnrollsThenSendsHeartbeat(t *testing.T) {
	var token string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/worker/enroll":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"worker": remote.Worker{ID: "worker-1"}, "token": "worker-token"})
		case "/api/v1/worker/heartbeat":
			token = r.Header.Get("Authorization")
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
	if err := client.Heartbeat(context.Background(), registered.Token, remote.WorkerCapabilities{Proxy: true}); err != nil {
		t.Fatal(err)
	}
	if token != "Bearer worker-token" {
		t.Fatalf("authorization=%q", token)
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
