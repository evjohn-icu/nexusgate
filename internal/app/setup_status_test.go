package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// newSetupStatusService builds a Service over a real, migrated SQLite
// repository — the NextStep progression is a claim about what the database
// actually reports, so the fake-repo pattern would prove nothing here — with a
// temp cache dir the disk probes can touch.
func newSetupStatusService(t *testing.T) *Service {
	t.Helper()
	ctx := context.Background()
	dir := secureDataDir(t)
	repo, err := sqlite.Open(filepath.Join(dir, "setup.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repo, config.Config{DataDir: dir, CacheDir: cacheDir, Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestSetupStatusFreshHubGuidesAddFootage(t *testing.T) {
	service := newSetupStatusService(t)
	status := service.SetupStatus(context.Background())
	if status.NextStep != SetupNextStepAddFootage {
		t.Fatalf("fresh hub: NextStep = %q, want %q", status.NextStep, SetupNextStepAddFootage)
	}
	if status.Ready {
		t.Fatal("fresh hub must not report ready")
	}
	if status.RootCount != 0 || status.ProviderCount != 0 || status.AssetCount != 0 {
		t.Fatalf("fresh hub: counts = roots %d providers %d assets %d, want all zero", status.RootCount, status.ProviderCount, status.AssetCount)
	}
	if !status.DataDirWritable || !status.CacheWritable {
		t.Fatalf("temp dirs must be writable: data_dir_writable=%v cache_writable=%v", status.DataDirWritable, status.CacheWritable)
	}
	if status.FreeDiskBytes <= 0 {
		t.Fatalf("free disk on the cache dir must be measurable and positive, got %d", status.FreeDiskBytes)
	}
	if !status.DBHealthy {
		t.Fatal("a freshly migrated database must pass integrity check")
	}
}

func TestSetupStatusRootThenProvidersAdvanceNextStep(t *testing.T) {
	service := newSetupStatusService(t)
	ctx := context.Background()
	rootDir := filepath.Join(t.TempDir(), "footage")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := service.repo.CreateLibraryRoot(ctx, rootDir); err != nil {
		t.Fatal(err)
	}
	status := service.SetupStatus(ctx)
	if status.RootCount != 1 {
		t.Fatalf("RootCount = %d, want 1", status.RootCount)
	}
	if status.NextStep != SetupNextStepConfigureProviders {
		t.Fatalf("root without providers: NextStep = %q, want %q", status.NextStep, SetupNextStepConfigureProviders)
	}
	if status.Ready {
		t.Fatal("a root with no provider must not report ready")
	}
}

// The binary booleans must track what exec.LookPath actually finds on this
// machine — a CI box without ffmpeg must see FFmpeg=false, one with it true.
func TestSetupStatusBinariesMatchLookPath(t *testing.T) {
	service := newSetupStatusService(t)
	status := service.SetupStatus(context.Background())
	for _, tc := range []struct {
		name  string
		got   bool
		field string
	}{
		{"ffmpeg", status.FFmpeg, "FFmpeg"},
		{"ffprobe", status.FFprobe, "FFprobe"},
		{"exiftool", status.ExifTool, "ExifTool"},
	} {
		_, err := exec.LookPath(tc.name)
		if tc.got != (err == nil) {
			t.Fatalf("%s = %v, but exec.LookPath(%q) error is %v", tc.field, tc.got, tc.name, err)
		}
	}
}

// TestSetupStatusRequiresHealthyRootRunnableVideoAndSearchableShots drives the
// whole first-run progression over a real, migrated SQLite repository. It pins
// that provider_ready counts only an enabled, runtime-supported video channel
// with an enabled secret-ready member (never a disabled, keyless or
// unsupported one), that healthy_root_count counts only healthy roots (never
// unknown or unavailable), that discovered/failed assets produce no searchable
// shots, and that a pending index keeps the hub in scan_or_process even with
// committed shots. Ready is verified against the conjunction with ExifTool
// deliberately absent (the page labels it optional).
func TestSetupStatusRequiresHealthyRootRunnableVideoAndSearchableShots(t *testing.T) {
	service := newSetupStatusService(t)
	ctx := context.Background()
	repo := service.repo.(*sqlite.Repository)

	// Three roots: a freshly created one is unknown (never scanned), one is
	// unavailable (unreachable mount), one is healthy.
	unknownRoot, err := repo.CreateLibraryRoot(ctx, filepath.Join(t.TempDir(), "unknown"))
	if err != nil {
		t.Fatal(err)
	}
	unavailableRoot, err := repo.CreateLibraryRoot(ctx, filepath.Join(t.TempDir(), "unavailable"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkRootUnavailable(ctx, unavailableRoot.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	healthyRoot, err := repo.CreateLibraryRoot(ctx, filepath.Join(t.TempDir(), "healthy"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkRootHealthy(ctx, healthyRoot.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	_ = unknownRoot

	status := service.SetupStatus(ctx)
	if status.RootCount != 3 || status.HealthyRootCount != 1 {
		t.Fatalf("roots = %d total / %d healthy, want 3 / 1", status.RootCount, status.HealthyRootCount)
	}
	if status.ProviderReady {
		t.Fatal("no channels: provider_ready must be false")
	}
	if status.NextStep != SetupNextStepConfigureProviders {
		t.Fatalf("no runnable video route: NextStep = %q, want configure_providers", status.NextStep)
	}
	if status.Ready {
		t.Fatal("a hub with no runnable video route must not report ready")
	}

	// An unsupported provider channel with a stored secret is not a runnable
	// video route: execution only routes names the runtime supports.
	unsupported := domain.ProviderChannel{
		Capability: "video_analysis", Label: "unsupported", ProviderName: "bogus_provider",
		Endpoint: "https://example.invalid", Model: "x", Enabled: true,
		Members: []domain.ProviderChannelMember{{Label: "primary", Enabled: true, Weight: 1, MaxInflight: 1}},
	}
	if _, err := service.SaveProviderChannel(ctx, unsupported, []string{"unsupported-key"}); err != nil {
		t.Fatal(err)
	}
	if status := service.SetupStatus(ctx); status.ProviderReady {
		t.Fatal("unsupported provider must not make provider_ready true")
	}

	// A saved-but-keyless enabled channel is not ready either.
	keyless := domain.ProviderChannel{
		Capability: "video_analysis", Label: "keyless", ProviderName: "gemini",
		Endpoint: "https://example.invalid", Model: "gemini-flash", Enabled: true,
		Members: []domain.ProviderChannelMember{{Label: "primary", Enabled: true, Weight: 1, MaxInflight: 1}},
	}
	if _, err := service.SaveProviderChannel(ctx, keyless, nil); err != nil {
		t.Fatal(err)
	}
	if status := service.SetupStatus(ctx); status.ProviderReady {
		t.Fatal("keyless channel must not make provider_ready true")
	}

	// A saved-but-disabled channel with a stored secret is not ready.
	disabled := domain.ProviderChannel{
		Capability: "video_analysis", Label: "disabled", ProviderName: "gemini",
		Endpoint: "https://example.invalid", Model: "gemini-flash", Enabled: false,
		Members: []domain.ProviderChannelMember{{Label: "primary", Enabled: true, Weight: 1, MaxInflight: 1}},
	}
	if _, err := service.SaveProviderChannel(ctx, disabled, []string{"disabled-key"}); err != nil {
		t.Fatal(err)
	}
	if status := service.SetupStatus(ctx); status.ProviderReady {
		t.Fatal("disabled channel must not make provider_ready true")
	}

	// An enabled, runtime-supported channel with an enabled secret-ready member
	// finally makes the video route runnable.
	good := domain.ProviderChannel{
		Capability: "video_analysis", Label: "good", ProviderName: "gemini",
		Endpoint: "https://example.invalid", Model: "gemini-flash", Enabled: true,
		Members: []domain.ProviderChannelMember{{Label: "primary", Enabled: true, Weight: 1, MaxInflight: 1}},
	}
	if _, err := service.SaveProviderChannel(ctx, good, []string{"good-key"}); err != nil {
		t.Fatal(err)
	}
	status = service.SetupStatus(ctx)
	if !status.ProviderReady {
		t.Fatal("enabled supported channel with a stored secret must make provider_ready true")
	}

	// A discovered-only asset and a failed asset (a job in failed state) must
	// not produce any searchable shot.
	seedRawSetupAsset(t, repo, "setup-discovered", "discovered", nil)
	seedRawSetupAsset(t, repo, "setup-failed", "discovered", &domain.Job{
		ID: "setup-failed-job", AssetID: "setup-failed", Type: "analyze", State: domain.JobFailed,
		Priority: 0, AttemptCount: 3, MaxAttempts: 3, RunAfter: time.Now().UTC().Add(-time.Hour),
		InputHash: "h", LastError: "fixture failure", Terminal: true,
	})
	status = service.SetupStatus(ctx)
	if status.AssetCount != 2 {
		t.Fatalf("AssetCount = %d, want 2 (discovered + failed)", status.AssetCount)
	}
	if status.SearchableShotCount != 0 {
		t.Fatalf("discovered/failed assets must not count as searchable shots, got %d", status.SearchableShotCount)
	}
	if status.NextStep != SetupNextStepScanOrProcess {
		t.Fatalf("healthy root + runnable provider + no shots: NextStep = %q, want scan_or_process", status.NextStep)
	}

	// Commit one shot through the canonical tables.
	seedCommittedSetupAsset(t, repo, "setup-committed")
	status = service.SetupStatus(ctx)
	if status.SearchableShotCount != 1 {
		t.Fatalf("SearchableShotCount = %d, want 1", status.SearchableShotCount)
	}
	if !status.SearchIndexReady {
		t.Fatal("a freshly migrated hub's index must be ready")
	}
	if status.NextStep != SetupNextStepSearch {
		t.Fatalf("healthy root + runnable provider + committed shot: NextStep = %q, want search", status.NextStep)
	}

	// A pending index drops back to scan_or_process and can never be Ready.
	if _, err := repo.DB().ExecContext(ctx, `UPDATE fts_index_state SET value='pending' WHERE name='cjk_bigram_v1'`); err != nil {
		t.Fatal(err)
	}
	status = service.SetupStatus(ctx)
	if status.SearchIndexReady {
		t.Fatal("a pending index must report search_index_ready=false")
	}
	if status.NextStep != SetupNextStepScanOrProcess {
		t.Fatalf("pending index: NextStep = %q, want scan_or_process", status.NextStep)
	}
	if status.Ready {
		t.Fatal("a hub with a pending index must not report ready")
	}
	if _, err := repo.DB().ExecContext(ctx, `UPDATE fts_index_state SET value='ready' WHERE name='cjk_bigram_v1'`); err != nil {
		t.Fatal(err)
	}

	// Ready excludes ExifTool from the conjunction (the page labels it
	// optional) and requires everything else.
	status = service.SetupStatus(ctx)
	_, ffmpegErr := exec.LookPath("ffmpeg")
	_, ffprobeErr := exec.LookPath("ffprobe")
	wantReady := ffmpegErr == nil && ffprobeErr == nil &&
		status.DataDirWritable && status.CacheWritable && status.FreeDiskOK && status.DBHealthy &&
		status.HealthyRootCount > 0 && status.ProviderReady &&
		status.SearchableShotCount > 0 && status.SearchIndexReady
	if status.Ready != wantReady {
		t.Fatalf("Ready = %v, want %v (ExifTool excluded from the conjunction)", status.Ready, wantReady)
	}
}

// seedRawSetupAsset inserts the minimum asset row, plus an optional failed job
// that makes the asset's processing status "failed" rather than "discovered".
func seedRawSetupAsset(t *testing.T, repo *sqlite.Repository, id, state string, failedJob *domain.Job) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,100,?,?,?)`, id, "fp-"+id, state, now, now); err != nil {
		t.Fatal(err)
	}
	if failedJob != nil {
		if _, err := repo.DB().ExecContext(ctx, `INSERT INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,last_error_message,terminal,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			failedJob.ID, failedJob.AssetID, failedJob.Type, failedJob.State, failedJob.Priority, failedJob.AttemptCount, failedJob.MaxAttempts, failedJob.RunAfter, failedJob.InputHash, failedJob.LastError, failedJob.Terminal, now, now); err != nil {
			t.Fatal(err)
		}
	}
}

// seedCommittedSetupAsset creates an asset that looks exactly like one whose
// analysis chain completed: it mirrors seedCommittedAsset in the sqlite tests
// so the canonical asset_shots row and the FKs it demands all exist.
func seedCommittedSetupAsset(t *testing.T, repo *sqlite.Repository, suffix string) {
	t.Helper()
	ctx := context.Background()
	assetID := "asset-" + suffix
	now := setupFormatTime(time.Now().UTC())
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,100,'ready',?,?)`, assetID, "fp-"+suffix, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES(?,?,?,?)`, "root-"+suffix, "/tmp/root-"+suffix, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO model_runs(id,asset_id,capability,provider,model,input_hash,prompt_version,schema_version,state,request_json,started_at) VALUES(?,'asset-'||?,'vision','qwen','qwen-vl','run-hash-'||?,'p','s','committed','{}',?)`,
		"run-"+suffix, suffix, suffix, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,?,?,?,?,1,1,?)`,
		"loc-"+suffix, assetID, "root-"+suffix, "clip.mov", "/tmp/clip-"+suffix+".mov", 123, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO media_metadata(asset_id,ffprobe_json,exiftool_json,normalized_json,probe_version,updated_at) VALUES(?,'{}','{}',?,'test',?)`, assetID, `{"duration_ms":600000,"has_audio":true}`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO asset_analysis(asset_id,source_run_id,schema_version,asset_type,shot_size,camera_motion,audio_type,lighting,people_count,has_speech,quality,summary,scene_tags_json,subjects_json,mood_tags_json,usable_as_json,quality_flags_json,extra_tags_json,editorial_reason,updated_at) VALUES(?,'run-'||?,'asset-analysis/v2','clip','close','static','none','day',0,0,'fine','summary','[]','[]','[]','[]','[]','[]','',?)`, assetID, suffix, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO asset_shots(id,asset_id,source_run_id,ordinal,start_ms,end_ms,description,tags_json,objects_json,actions_json,mood_json,confidence,created_at) VALUES(?,?,'run-'||?,0,0,1000,'a committed shot','[]','[]','[]','[]',0.9,?)`, "shot-"+suffix, assetID, suffix, now); err != nil {
		t.Fatal(err)
	}
}

// setupFormatTime is the local mirror of the sqlite package's unexported
// formatTime: the timestamp columns are stored in this exact layout, so any
// row seeded with a raw time.Time must use it.
func setupFormatTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}
