package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/app"
	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
)

type scanRequestContextRepo struct {
	*sqlite.Repository
	entered chan context.Context
	release chan struct{}
	once    sync.Once
}

func (r *scanRequestContextRepo) UpsertScannedFile(ctx context.Context, root domain.LibraryRoot, relativePath, absolutePath string, info fs.FileInfo, fingerprint string) (domain.ScannedFile, error) {
	r.once.Do(func() { r.entered <- ctx })
	<-r.release
	return r.Repository.UpsertScannedFile(ctx, root, relativePath, absolutePath, info, fingerprint)
}

func newScanRequestContextFixture(t *testing.T) (*app.Service, *scanRequestContextRepo, domain.LibraryRoot) {
	t.Helper()
	repository, err := sqlite.Open(filepath.Join(t.TempDir(), "scan-request-context.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repository.Close() })
	if err := repository.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	repo := &scanRequestContextRepo{
		Repository: repository,
		entered:    make(chan context.Context, 1),
		release:    make(chan struct{}),
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	rootPath := t.TempDir()
	for _, name := range []string{"first.mp4", "second.mp4"} {
		if err := writeScanRequestFixture(rootPath, name); err != nil {
			t.Fatal(err)
		}
	}
	root, err := service.AddLibraryRoot(context.Background(), rootPath)
	if err != nil {
		t.Fatal(err)
	}
	return service, repo, root
}

func writeScanRequestFixture(rootPath, name string) error {
	return os.WriteFile(filepath.Join(rootPath, name), []byte(name), 0o600)
}

func TestScanRequestContextSurvivesCancellation(t *testing.T) {
	service, repo, root := newScanRequestContextFixture(t)

	requestContext, cancel := context.WithCancel(context.Background())
	request := hubAdminRequest(service, http.MethodPost, "/api/v1/roots/"+root.ID+"/scan", nil).WithContext(requestContext)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		NewServer("", service).Handler().ServeHTTP(response, request)
	}()

	var scanContext context.Context
	select {
	case scanContext = <-repo.entered:
	case <-time.After(time.Second):
		t.Fatal("scan did not reach the blocking repository fake")
	}
	cancel()
	select {
	case <-scanContext.Done():
		t.Fatal("scan context was cancelled with the request")
	default:
	}
	close(repo.release)
	<-done

	if response.Code != http.StatusOK {
		t.Fatalf("scan status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Discovered     int    `json:"discovered"`
		PipelineStatus string `json:"pipeline_status"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Discovered != 2 {
		t.Fatalf("scan discovered=%d, want both files after request cancellation", body.Discovered)
	}
	if body.PipelineStatus != "started" {
		t.Fatalf("pipeline_status=%q, want started", body.PipelineStatus)
	}
}

func TestScanRequestContextHasLivenessDeadline(t *testing.T) {
	service, repo, root := newScanRequestContextFixture(t)

	request := hubAdminRequest(service, http.MethodPost, "/api/v1/roots/"+root.ID+"/scan", nil)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		NewServer("", service).Handler().ServeHTTP(response, request)
	}()

	select {
	case scanContext := <-repo.entered:
		deadline, ok := scanContext.Deadline()
		if !ok {
			t.Fatal("scan context has no liveness deadline")
		}
		remaining := time.Until(deadline)
		if remaining < scanRootLivenessBound-2*time.Second || remaining > scanRootLivenessBound {
			t.Fatalf("scan deadline is %v away, want approximately %v", remaining, scanRootLivenessBound)
		}
	case <-time.After(time.Second):
		t.Fatal("scan did not reach the blocking repository fake")
	}
	close(repo.release)
	<-done
	if response.Code != http.StatusOK {
		t.Fatalf("scan status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestScanRootFastScanKeepsPipelineStatusJSON(t *testing.T) {
	service, repo, root := newScanRequestContextFixture(t)
	close(repo.release)

	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/roots/"+root.ID+"/scan", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("scan status=%d body=%s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["pipeline_status"] != "started" {
		t.Fatalf("pipeline_status=%v, want started", body["pipeline_status"])
	}
}
