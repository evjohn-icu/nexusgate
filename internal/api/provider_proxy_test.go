package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/app"
	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/remote"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
)

func TestWorkerProviderProxyRequiresLeaseAndKeepsKeysServerSide(t *testing.T) {
	ctx := context.Background()
	const providerSecret = "proxy-provider-secret"
	const extraSecret = "proxy-extra-header-secret"
	const urlUser = "proxy-url-user"
	const urlPassword = "proxy-url-password"
	var receivedBody []byte
	var receivedAuth string
	var receivedExtra string
	var receivedURLQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		receivedExtra = r.Header.Get("X-Provider-Secret")
		receivedURLQuery = r.URL.Query().Get("user_key")
		receivedBody, _ = io.ReadAll(io.LimitReader(r.Body, 2<<20))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"summary":"` + providerSecret + `","extra":"` + extraSecret + `","url_user":"` + urlUser + `","url_password":"` + urlPassword + `","ordinary":"ordinary-value","nested":"{\"token\":\"` + providerSecret + `\"}"}`))
	}))
	defer upstream.Close()
	providerURL := strings.Replace(upstream.URL, "http://", "http://"+urlUser+":"+urlPassword+"@", 1) + "?user_key=url-query-secret"

	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "provider-proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	seedRemoteAnalyzeJob(t, ctx, repo)
	pairing, err := repo.CreateWorkerPairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	worker, token, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{Name: "proxy-worker", Platform: "linux-amd64", Capabilities: remote.WorkerCapabilities{ProviderOperations: []string{"video_analysis"}}})
	if err != nil {
		t.Fatal(err)
	}
	job, err := repo.LeaseNextJob(ctx, worker.ID, nil, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("lease=%+v err=%v", job, err)
	}

	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Providers: config.ProvidersConfig{
		VisionPrimary: "volcengine_video",
		VolcVideo:     config.ProviderConfig{Enabled: true, BaseURL: providerURL, Path: "/v1/chat/completions", APIKey: providerSecret, Model: "vision-v1", AuthHeader: "Authorization", AuthScheme: "Bearer", ExtraHeaders: map[string]string{"X-Provider-Secret": extraSecret}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()
	payload := []byte(`{"input":"find the city shot"}`)

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/api/v1/worker/jobs/"+job.ID+"/provider/video_analysis", bytes.NewReader(payload)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized proxy status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/worker/jobs/"+job.ID+"/provider/video_analysis", bytes.NewReader(payload))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("proxy status=%d body=%s", response.Code, response.Body.String())
	}
	if receivedAuth != "Bearer "+providerSecret {
		t.Fatalf("upstream did not receive configured auth header: %q", receivedAuth)
	}
	if receivedExtra != extraSecret {
		t.Fatalf("upstream did not receive extra header credential: %q", receivedExtra)
	}
	if receivedURLQuery != "url-query-secret" {
		t.Fatalf("upstream did not receive URL query credential: %q", receivedURLQuery)
	}
	if string(receivedBody) != string(payload) {
		t.Fatalf("upstream body=%s want=%s", receivedBody, payload)
	}
	if strings.Contains(response.Body.String(), providerSecret) {
		t.Fatalf("proxy response leaked provider secret: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "[REDACTED]") {
		t.Fatalf("proxy response did not redact echoed secret: %s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), extraSecret) || strings.Contains(response.Body.String(), urlUser) || strings.Contains(response.Body.String(), urlPassword) || strings.Contains(response.Body.String(), "url-query-secret") {
		t.Fatalf("proxy response leaked a non-API credential: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "ordinary-value") {
		t.Fatalf("proxy response changed ordinary value: %s", response.Body.String())
	}
	if !json.Valid(response.Body.Bytes()) {
		t.Fatalf("proxy response is not valid JSON: %s", response.Body.String())
	}
}

func TestWorkerHeartbeatCannotEscalateProviderOperations(t *testing.T) {
	ctx := context.Background()
	var upstreamRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "provider-escalation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	seedRemoteAnalyzeJob(t, ctx, repo)
	pairing, err := repo.CreateWorkerPairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	worker, token, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{
		Name: "untrusted-provider-worker", Platform: "linux-amd64",
		Capabilities: remote.WorkerCapabilities{Proxy: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := repo.LeaseNextJob(ctx, worker.ID, nil, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("lease=%+v err=%v", job, err)
	}

	service, err := app.NewService(repo, config.Config{
		DataDir:     secureTestDataDir(t),
		HubSecurity: config.HubSecurityConfig{AllowWorkerProviderCredentials: true},
		Providers: config.ProvidersConfig{VisionPrimary: "volcengine_video", VolcVideo: config.ProviderConfig{
			Enabled: true, BaseURL: upstream.URL, Path: "/v1/chat/completions", APIKey: "secret", Model: "vision-v1",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()
	heartbeat := httptest.NewRequest(http.MethodPost, "/api/v1/worker/heartbeat", strings.NewReader(`{"capabilities":{"provider_operations":["video_analysis"]}}`))
	heartbeat.Header.Set("Authorization", "Bearer "+token)
	heartbeat.Header.Set("Content-Type", "application/json")
	heartbeatResponse := httptest.NewRecorder()
	handler.ServeHTTP(heartbeatResponse, heartbeat)
	if heartbeatResponse.Code != http.StatusNoContent {
		t.Fatalf("heartbeat status=%d body=%s", heartbeatResponse.Code, heartbeatResponse.Body.String())
	}

	credential := httptest.NewRequest(http.MethodPost, "/api/v1/worker/jobs/"+job.ID+"/credentials/video_analysis", nil)
	credential.Header.Set("Authorization", "Bearer "+token)
	credentialResponse := httptest.NewRecorder()
	handler.ServeHTTP(credentialResponse, credential)
	if credentialResponse.Code != http.StatusBadRequest {
		t.Fatalf("escalated credential status=%d body=%s", credentialResponse.Code, credentialResponse.Body.String())
	}

	proxy := httptest.NewRequest(http.MethodPost, "/api/v1/worker/jobs/"+job.ID+"/provider/video_analysis", strings.NewReader(`{"input":"should not reach provider"}`))
	proxy.Header.Set("Authorization", "Bearer "+token)
	proxy.Header.Set("Content-Type", "application/json")
	proxyResponse := httptest.NewRecorder()
	handler.ServeHTTP(proxyResponse, proxy)
	if proxyResponse.Code != http.StatusBadRequest {
		t.Fatalf("escalated proxy status=%d body=%s", proxyResponse.Code, proxyResponse.Body.String())
	}
	if requests := upstreamRequests.Load(); requests != 0 {
		t.Fatalf("upstream received %d requests after heartbeat escalation", requests)
	}
}

func TestWorkerProviderProxyRejectsMediaAndOversizedBodies(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "provider-proxy-limits.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	seedRemoteAnalyzeJob(t, ctx, repo)
	pairing, _ := repo.CreateWorkerPairing(ctx, time.Minute)
	worker, token, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{Name: "proxy-limits", Platform: "linux", Capabilities: remote.WorkerCapabilities{ProviderOperations: []string{"video_analysis"}}})
	if err != nil {
		t.Fatal(err)
	}
	job, err := repo.LeaseNextJob(ctx, worker.ID, nil, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("lease=%+v err=%v", job, err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Providers: config.ProvidersConfig{VisionPrimary: "volcengine_video", VolcVideo: config.ProviderConfig{Enabled: true, BaseURL: "https://provider.invalid", APIKey: "not-returned", Model: "vision-v1"}}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()
	for _, contentType := range []string{"multipart/form-data; boundary=abc", "audio/wav", "video/mp4", "image/jpeg"} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/worker/jobs/"+job.ID+"/provider/video_analysis", strings.NewReader("media"))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", contentType)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("content type %q status=%d body=%s", contentType, response.Code, response.Body.String())
		}
	}
	oversized := bytes.Repeat([]byte("x"), 2<<20+1)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/worker/jobs/"+job.ID+"/provider/video_analysis", bytes.NewReader(oversized))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized proxy status=%d body=%s", response.Code, response.Body.String())
	}
	// A valid JSON document one byte below the limit must pass the API size
	// guard, even though this fixture endpoint is intentionally unreachable.
	under := []byte(`{"x":"` + strings.Repeat("a", int(app.MaxProviderProxyBodyBytes())-len(`{"x":""}`)-1) + `"}`)
	request = httptest.NewRequest(http.MethodPost, "/api/v1/worker/jobs/"+job.ID+"/provider/video_analysis", bytes.NewReader(under))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("2MiB-1 proxy request was rejected as oversized: %d", response.Code)
	}
}

func TestWorkerProviderProxyAuthSchemes(t *testing.T) {
	for _, tc := range []struct{ name, scheme, want string }{
		{"default bearer", "", "Bearer secret-auth-key"},
		{"raw", "raw", "secret-auth-key"},
		{"custom", "Token", "Token secret-auth-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var got string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer upstream.Close()
			repo, err := sqlite.Open(filepath.Join(t.TempDir(), "auth.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer repo.Close()
			if err := repo.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			seedRemoteAnalyzeJob(t, ctx, repo)
			pairing, _ := repo.CreateWorkerPairing(ctx, time.Minute)
			worker, token, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{Name: tc.name, Platform: "linux", Capabilities: remote.WorkerCapabilities{ProviderOperations: []string{"video_analysis"}}})
			if err != nil {
				t.Fatal(err)
			}
			job, err := repo.LeaseNextJob(ctx, worker.ID, nil, domain.LeaseFilter{})
			if err != nil || job == nil {
				t.Fatalf("lease=%+v err=%v", job, err)
			}
			service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Providers: config.ProvidersConfig{VisionPrimary: "volcengine_video", VolcVideo: config.ProviderConfig{Enabled: true, BaseURL: upstream.URL, APIKey: "secret-auth-key", AuthScheme: tc.scheme}}})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/worker/jobs/"+job.ID+"/provider/video_analysis", strings.NewReader(`{"x":1}`))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			NewServer("", service).Handler().ServeHTTP(response, req)
			if response.Code != http.StatusOK || got != tc.want {
				t.Fatalf("status=%d auth=%q want=%q", response.Code, got, tc.want)
			}
		})
	}
}

func TestWorkerProviderProxyRejectsMalformedUpstreamJSON(t *testing.T) {
	for _, body := range []string{"not-json", `{"broken":`} {
		t.Run(strings.ReplaceAll(body, "{", "object-"), func(t *testing.T) {
			ctx := context.Background()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			defer upstream.Close()
			repo, service, token, job := proxyFixture(t, ctx, config.ProviderConfig{Enabled: true, BaseURL: upstream.URL, APIKey: "malformed-key"})
			defer repo.Close()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/worker/jobs/"+job.ID+"/provider/video_analysis", strings.NewReader(`{"x":1}`))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			NewServer("", service).Handler().ServeHTTP(response, req)
			if response.Code != http.StatusBadGateway {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func proxyFixture(t *testing.T, ctx context.Context, provider config.ProviderConfig) (*sqlite.Repository, *app.Service, string, *domain.Job) {
	t.Helper()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "fixture.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	seedRemoteAnalyzeJob(t, ctx, repo)
	pairing, err := repo.CreateWorkerPairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	worker, token, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{Name: "proxy-fixture", Platform: "linux", Capabilities: remote.WorkerCapabilities{ProviderOperations: []string{"video_analysis"}}})
	if err != nil {
		t.Fatal(err)
	}
	job, err := repo.LeaseNextJob(ctx, worker.ID, nil, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("lease=%+v err=%v", job, err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Providers: config.ProvidersConfig{VisionPrimary: "volcengine_video", VolcVideo: provider}})
	if err != nil {
		t.Fatal(err)
	}
	return repo, service, token, job
}
