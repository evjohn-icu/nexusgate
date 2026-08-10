package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// storageOverviewService builds a real sqlite-backed service whose DataDir is
// also where the database and cache live, so the overview's file-side numbers
// (database, cache classification) are observable rather than zeroed.
func storageOverviewService(t *testing.T, dbName string) (*app.Service, *sqlite.Repository, string) {
	t.Helper()
	dataDir := secureTestDataDir(t)
	repo, err := sqlite.Open(filepath.Join(dataDir, dbName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{
		DataDir:      dataDir,
		CacheDir:     filepath.Join(dataDir, "cache"),
		DatabasePath: filepath.Join(dataDir, dbName),
		Hardware:     media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, repo, dataDir
}

// TestStorageOverviewEndpointPinsJSONShape seeds real rows and cache files and
// asserts the endpoint reports exactly what is on disk, labelled by the same
// names the settings page renders. The endpoint is a trusted read: no token
// needed on the LAN, like /api/v1/jobs/summary.
func TestStorageOverviewEndpointPinsJSONShape(t *testing.T) {
	service, repo, dataDir := storageOverviewService(t, "storage-overview.db")

	ctx := context.Background()
	now := time.Now().UTC()
	// Two assets: the SUM lands in original_estimate_bytes.
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-a','fp-a',150,'discovered',?,?),('asset-b','fp-b',350,'discovered',?,?)`, now, now, now, now); err != nil {
		t.Fatal(err)
	}

	// Cache layout as the pipeline writes it: per-asset dirs with
	// thumbnail-<mode>.jpg / proxy-<mode>.mp4 / audio.m4a, plus sources/ and
	// analysis scratch, plus one unclassifiable leftover.
	writeCache := func(rel string, size int) {
		t.Helper()
		path := filepath.Join(dataDir, "cache", rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeCache("asset-a/thumbnail-software.jpg", 1000)
	writeCache("asset-a/proxy-software.mp4", 5000)
	writeCache("asset-a/audio.m4a", 700)
	writeCache("asset-a/analysis-frames/shot-1.png", 300)
	writeCache("sources/asset-a/1.MOV", 9000)
	writeCache("leftover.bin", 42)

	recorder := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(recorder, lanRequest(http.MethodGet, "/api/v1/storage/overview", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var overview struct {
		OriginalEstimateBytes int64 `json:"original_estimate_bytes"`
		DerivedBytes          int64 `json:"derived_bytes"`
		ThumbnailBytes        int64 `json:"thumbnail_bytes"`
		ProxyBytes            int64 `json:"proxy_bytes"`
		AudioBytes            int64 `json:"audio_bytes"`
		SourceStagingBytes    int64 `json:"source_staging_bytes"`
		ScratchBytes          int64 `json:"scratch_bytes"`
		DatabaseBytes         int64 `json:"database_bytes"`
		TemporaryBytes        int64 `json:"temporary_bytes"`
		FreeDiskBytes         int64 `json:"free_disk_bytes"`
		RebuildableBytes      int64 `json:"rebuildable_bytes"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &overview); err != nil {
		t.Fatalf("decode: %v body=%s", err, recorder.Body.String())
	}
	if overview.OriginalEstimateBytes != 500 {
		t.Fatalf("original_estimate_bytes = %d, want 500", overview.OriginalEstimateBytes)
	}
	if overview.ThumbnailBytes != 1000 || overview.ProxyBytes != 5000 || overview.AudioBytes != 700 {
		t.Fatalf("derive split wrong: thumb=%d proxy=%d audio=%d", overview.ThumbnailBytes, overview.ProxyBytes, overview.AudioBytes)
	}
	if overview.DerivedBytes != 6700 {
		t.Fatalf("derived = %d, want 6700", overview.DerivedBytes)
	}
	// Rebuildable = derived + scratch (J1a's cache.RebuildableBytes): a
	// pipeline re-run regenerates analysis frames too.
	if overview.RebuildableBytes != 7000 {
		t.Fatalf("rebuildable = %d, want 7000", overview.RebuildableBytes)
	}
	if overview.SourceStagingBytes != 9000 {
		t.Fatalf("source_staging_bytes = %d, want 9000", overview.SourceStagingBytes)
	}
	if overview.ScratchBytes != 300 {
		t.Fatalf("scratch_bytes = %d, want 300", overview.ScratchBytes)
	}
	if overview.TemporaryBytes != 342 { // scratch 300 + leftover 42
		t.Fatalf("temporary_bytes = %d, want 342", overview.TemporaryBytes)
	}
	if overview.DatabaseBytes <= 0 {
		t.Fatalf("database_bytes = %d, want >0", overview.DatabaseBytes)
	}
	if overview.FreeDiskBytes <= 0 {
		t.Fatalf("free_disk_bytes = %d, want >0", overview.FreeDiskBytes)
	}
}

// TestSettingsPageRendersStorageOverviewPanel pins the new panel's markers so
// a stale anchor or removed fetch line cannot silently drop the feature.
func TestSettingsPageRendersStorageOverviewPanel(t *testing.T) {
	service, _, _ := storageOverviewService(t, "settings-storage.db")
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	page := response.Body.String()
	for _, marker := range []string{
		"存储概览",                     // the panel itself
		"可安全释放（可重建）",               // the rebuildable row
		"估算（只读）",                   // the original-media row's honesty label
		"/api/v1/storage/overview", // the fetch target
		"loadStorage",              // and it actually loads
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("settings page is missing %q", marker)
		}
	}
}
