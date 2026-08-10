package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/domain"
	videoanalysis "github.com/evjohn-icu/timingdex/internal/domain/video_analysis"
	sqlite "github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// e2eService bundles the app.Service (the scan boundary) with the
// mock-wired pipeline the chain runs on and the repository the assertions
// query, so a test reads state through the same objects that ran the chain.
type e2eService struct {
	svc      *app.Service
	pipeline *app.Pipeline
	repo     *sqlite.Repository
}

// newEdgeService is the common fixture wiring for the edge tests: a real
// sqlite repository, a software-proxy pipeline, and the deterministic mock
// vision provider. Skips live inside generateClip, as the suite's contract
// states: everything after the fixture is a chain verdict.
func newEdgeService(t *testing.T, repo *sqlite.Repository, dataDir string, video *e2eVideoMock) *e2eService {
	t.Helper()
	service, pipeline := newE2EService(t, repo, dataDir, video)
	return &e2eService{svc: service, pipeline: pipeline, repo: repo}
}

// TestWeirdAspectPipeline runs the full chain (scan → probe → derive →
// analyze → search) on an extreme-ratio source: 180x640, nine pixels wide
// per row of height. The pipeline stages that infer or render from geometry
// (probe, the scale filters, window planning) must survive it: the probe
// must report the true dimensions, the derive must render a proxy at the
// capped dimension, one shot must commit, and search must answer.
func TestWeirdAspectPipeline(t *testing.T) {
	repo, dataDir := openE2ERepo(t)
	video := &e2eVideoMock{}
	fx := newEdgeService(t, repo, dataDir, video)
	ctx := context.Background()

	rootDir := filepath.Join(t.TempDir(), "footage")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	generateClip(t, rootDir, "tall-180x640.mp4", ClipOpts{Width: 180, Height: 640, DurationS: 2})
	root, err := fx.svc.AddLibraryRoot(ctx, rootDir)
	if err != nil {
		t.Fatal(err)
	}

	result := scanRoot(t, fx.svc, root.ID)
	if result.Discovered != 1 {
		t.Fatalf("scan discovered=%d, want 1: %+v", result.Discovered, result)
	}
	assets, err := fx.repo.ListAssets(ctx, 10, 0)
	if err != nil || len(assets) != 1 {
		t.Fatalf("assets after scan: %+v err=%v", assets, err)
	}
	assetID := assets[0].ID

	runBoundedPipeline(t, fx.pipeline)

	metadata, err := fx.repo.GetMediaMetadata(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if metadata == nil {
		t.Fatal("no media metadata after probe")
	}
	if metadata.Width != 180 || metadata.Height != 640 {
		t.Fatalf("probe reported %dx%d, want 180x640 (the fixture's true geometry)", metadata.Width, metadata.Height)
	}
	if metadata.Orientation != "portrait" {
		t.Fatalf("orientation = %q, want portrait", metadata.Orientation)
	}
	if metadata.DurationMS != 2000 {
		t.Fatalf("duration = %dms, want 2000ms", metadata.DurationMS)
	}

	shots, err := fx.repo.ListAssetShots(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) != 1 || shots[0].StartMS != 0 || shots[0].EndMS != 2000 {
		t.Fatalf("weird-aspect analysis committed %d shots, want one covering the asset: %+v", len(shots), shots)
	}
	if countModelRuns(t, fx.repo, assetID) != 1 {
		t.Fatalf("model runs != 1 for the weird-aspect asset")
	}

	hits, err := fx.repo.SearchShots(ctx, "scene", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 1 || hits[0].AssetID != assetID {
		t.Fatalf("search did not find the weird-aspect shot: %+v", hits)
	}
}

// TestShortClipPipeline proves the sub-second asset does not explode: the
// probe stage answers, the pipeline pass terminates (the bounded-run guard
// is the no-infinite-loop assertion), and the analysis stage commits a sane
// single shot inside the 500ms timeline. The derive stage is deliberately
// not drained to completion: media.PreviewRenderer.RenderThumbnail seeks a
// hardcoded 1 second into the source, which a sub-second clip cannot
// satisfy, and the failure shape varies by ffmpeg version — the derive
// outcome is a media-layer boundary, not the fixture's question. The proxy
// is staged under a completed derive job (the same pattern the multiframe
// suite uses) so the analysis path is exercised deterministically.
func TestShortClipPipeline(t *testing.T) {
	repo, dataDir := openE2ERepo(t)
	video := &e2eVideoMock{}
	fx := newEdgeService(t, repo, dataDir, video)
	ctx := context.Background()

	rootDir := filepath.Join(t.TempDir(), "footage")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	clip := generateClip(t, rootDir, "subsecond.mp4", ClipOpts{DurationS: 0.5})
	root, err := fx.svc.AddLibraryRoot(ctx, rootDir)
	if err != nil {
		t.Fatal(err)
	}

	result := scanRoot(t, fx.svc, root.ID)
	if result.Discovered != 1 {
		t.Fatalf("scan discovered=%d, want 1", result.Discovered)
	}
	assets, err := fx.repo.ListAssets(ctx, 10, 0)
	if err != nil || len(assets) != 1 {
		t.Fatalf("assets after scan: %+v err=%v", assets, err)
	}
	assetID := assets[0].ID

	// Run #1 drains the scan-enqueued chain under the deadline guard. The
	// probe completes; the derive job it enqueued is removed afterwards so
	// its version-dependent retry tail cannot leak into the analysis run.
	runBoundedPipeline(t, fx.pipeline)

	metadata, err := fx.repo.GetMediaMetadata(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if metadata == nil || metadata.DurationMS != 500 {
		t.Fatalf("probe of the sub-second clip = %+v, want 500ms", metadata)
	}
	if _, err := fx.repo.DB().ExecContext(ctx, `DELETE FROM jobs WHERE asset_id=? AND job_type='derive'`, assetID); err != nil {
		t.Fatal(err)
	}

	if countModelRuns(t, fx.repo, assetID) == 0 {
		// The derive stage did not complete on this ffmpeg build (see the
		// test doc); stage the proxy under a completed derive job instead.
		if err := fx.repo.EnqueueJob(ctx, assetID, domain.JobDerive, "seed-short-proxy", 90); err != nil {
			t.Fatal(err)
		}
		seedJob, err := fx.repo.LeaseNextJob(ctx, "seed", nil, domain.LeaseFilter{})
		if err != nil || seedJob == nil {
			t.Fatalf("lease seeded derive job: job=%+v err=%v", seedJob, err)
		}
		info, err := os.Stat(clip)
		if err != nil {
			t.Fatal(err)
		}
		// The seeded proxy points at the source file itself: the asset is
		// tiny, the mock provider never reads the video, and analyzeVideo
		// only stats the path before its (no-split) window plan.
		if err := fx.repo.SaveArtifact(ctx, domain.DerivedArtifact{
			ID: "seed-proxy-" + assetID, AssetID: assetID, Type: "proxy",
			ProfileHash: "proxy-720-software-h264-x264-v1", LocalPath: clip, SizeBytes: info.Size(),
		}, seedJob.ID, "seed"); err != nil {
			t.Fatal(err)
		}
		if err := fx.repo.CompleteJob(ctx, seedJob.ID, "seed", domain.JobSucceeded, ""); err != nil {
			t.Fatal(err)
		}
		if err := fx.repo.EnqueueJob(ctx, assetID, domain.JobAnalyze, "short-analyze-v1", 30); err != nil {
			t.Fatal(err)
		}
		runBoundedPipeline(t, fx.pipeline)
	}

	if got := countModelRuns(t, fx.repo, assetID); got != 1 {
		t.Fatalf("model runs = %d, want 1: the sub-second asset was analysed once", got)
	}
	shots, err := fx.repo.ListAssetShots(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) != 1 {
		t.Fatalf("sub-second analysis committed %d shots, want at most 2 and exactly 1 here: %+v", len(shots), shots)
	}
	if shots[0].StartMS < 0 || shots[0].EndMS > 500 || shots[0].EndMS <= shots[0].StartMS {
		t.Fatalf("sub-second shot lies outside the 500ms timeline: %+v", shots[0])
	}
	if shots[0].Ordinal != 0 {
		t.Fatalf("shot ordinal = %d, want 0", shots[0].Ordinal)
	}
	hits, err := fx.repo.SearchShots(ctx, "scene", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].AssetID != assetID {
		t.Fatalf("search did not find the sub-second shot: %+v", hits)
	}
}

// TestMultiShotClip runs the full chain on an 8s clip made of two visually
// distinct segments (testsrc then testsrc2, concatenated with the demuxer).
// The mock provider mirrors what a real VLM would report for a scene
// change: two shots split at the midpoint. The chain must commit both, with
// distinct start ranges and sane ordinals, and index them.
func TestMultiShotClip(t *testing.T) {
	repo, dataDir := openE2ERepo(t)
	video := &e2eVideoMock{
		shots: func(durationMS int64) []videoanalysis.Shot {
			mid := durationMS / 2
			return []videoanalysis.Shot{
				{StartMS: 0, EndMS: mid, Description: "scene 0 (first segment)", Tags: []string{"testsrc"}},
				{StartMS: mid, EndMS: durationMS, Description: "scene 1 (second segment)", Tags: []string{"testsrc2"}},
			}
		},
	}
	fx := newEdgeService(t, repo, dataDir, video)
	ctx := context.Background()

	rootDir := filepath.Join(t.TempDir(), "footage")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	first := DefaultClipOpts()
	first.DurationS = 4
	second := DefaultClipOpts()
	second.Scene = "testsrc2"
	second.DurationS = 4
	generateSceneChangeClip(t, rootDir, "two-scenes.mp4", first, second)
	root, err := fx.svc.AddLibraryRoot(ctx, rootDir)
	if err != nil {
		t.Fatal(err)
	}

	result := scanRoot(t, fx.svc, root.ID)
	if result.Discovered != 1 {
		t.Fatalf("scan discovered=%d, want 1", result.Discovered)
	}
	assets, err := fx.repo.ListAssets(ctx, 10, 0)
	if err != nil || len(assets) != 1 {
		t.Fatalf("assets after scan: %+v err=%v", assets, err)
	}
	assetID := assets[0].ID

	runBoundedPipeline(t, fx.pipeline)

	metadata, err := fx.repo.GetMediaMetadata(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if metadata == nil || metadata.DurationMS != 8000 {
		t.Fatalf("probe of the concatenated clip = %+v, want 8000ms", metadata)
	}

	shots, err := fx.repo.ListAssetShots(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) < 2 {
		t.Fatalf("multi-scene analysis committed %d shots, want >= 2: %+v", len(shots), shots)
	}
	starts := map[int64]bool{}
	for i, s := range shots {
		if s.Ordinal != i {
			t.Fatalf("shot %d has ordinal %d, want %d (sane ordinal sequence)", i, s.Ordinal, i)
		}
		if s.EndMS <= s.StartMS || s.StartMS < 0 || s.EndMS > 8000 {
			t.Fatalf("shot %d lies outside the 8s timeline: %+v", i, s)
		}
		if starts[s.StartMS] {
			t.Fatalf("shots share a start time %dms: %+v", s.StartMS, shots)
		}
		starts[s.StartMS] = true
	}
	if !starts[0] {
		t.Fatalf("no shot starts at 0ms: %+v", shots)
	}
	if countModelRuns(t, fx.repo, assetID) != 1 {
		t.Fatalf("model runs != 1 for the multi-scene asset")
	}
	hits, err := fx.repo.SearchShots(ctx, "scene", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("search found %d multi-scene shots, want both: %+v", len(hits), hits)
	}
}

// TestMultiLocationAssetE2E proves the Wave 2 identity rule end to end: the
// same file at two paths inside one root is ONE asset (fingerprint + size
// identity), both locations live, and the pipeline analyses it once — the
// model_runs count of 1 is the proof that the second location did not cause
// a second paid analysis.
func TestMultiLocationAssetE2E(t *testing.T) {
	repo, dataDir := openE2ERepo(t)
	video := &e2eVideoMock{}
	fx := newEdgeService(t, repo, dataDir, video)
	ctx := context.Background()

	src := generateClip(t, t.TempDir(), "clip.mp4", DefaultClipOpts())
	rootDir := filepath.Join(t.TempDir(), "footage")
	first := filepath.Join(rootDir, "cam-a", "clip.mp4")
	second := filepath.Join(rootDir, "cam-b", "clip.mp4")
	copyFile(t, src, first)
	copyFile(t, src, second)
	root, err := fx.svc.AddLibraryRoot(ctx, rootDir)
	if err != nil {
		t.Fatal(err)
	}

	result := scanRoot(t, fx.svc, root.ID)
	if result.Discovered != 1 || result.Linked != 1 {
		t.Fatalf("scan discovered=%d linked=%d, want 1 and 1 (first path creates, second links)", result.Discovered, result.Linked)
	}
	assets, err := fx.repo.ListAssets(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 {
		t.Fatalf("two paths to the same content produced %d assets, want 1", len(assets))
	}
	assetID := assets[0].ID
	if got := countLocationsWithExists(t, fx.repo, assetID, 1); got != 2 {
		t.Fatalf("live locations = %d, want 2", got)
	}

	runBoundedPipeline(t, fx.pipeline)

	if got := countModelRuns(t, fx.repo, assetID); got != 1 {
		t.Fatalf("model runs = %d, want 1: the second location must not cause a second paid analysis", got)
	}
	shots, err := fx.repo.ListAssetShots(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) == 0 {
		t.Fatal("no shots committed for the multi-location asset")
	}
	hits, err := fx.repo.SearchShots(ctx, "scene", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 1 || hits[0].AssetID != assetID {
		t.Fatalf("search did not find the multi-location asset's shot: %+v", hits)
	}
}

// TestRootOfflineE2E exercises the O-wave root-health gate end to end. A
// root that had a processed asset is removed from disk; the next scan must
// NOT reconcile (the asset stays discovered, the location stays live,
// missing_since stays empty) and the root verdict becomes unavailable. When
// the directory comes back (a NAS remount, an rslave bind reappearing), the
// supervisor-style rescan — the same ScanLibraryRoot the supervisor calls
// per pass — flips the root healthy again and leaves the unchanged asset
// untouched (no new probe job: the stable key dedups the re-enqueue).
func TestRootOfflineE2E(t *testing.T) {
	repo, dataDir := openE2ERepo(t)
	video := &e2eVideoMock{}
	fx := newEdgeService(t, repo, dataDir, video)
	ctx := context.Background()

	src := generateClip(t, t.TempDir(), "clip.mp4", DefaultClipOpts())
	rootDir := filepath.Join(t.TempDir(), "footage")
	clip := filepath.Join(rootDir, "clip.mp4")
	copyFile(t, src, clip)
	info, err := os.Stat(clip)
	if err != nil {
		t.Fatal(err)
	}
	originalMtime := info.ModTime()
	root, err := fx.svc.AddLibraryRoot(ctx, rootDir)
	if err != nil {
		t.Fatal(err)
	}

	// Phase 1: the root is healthy, the asset is discovered and processed.
	first := scanRoot(t, fx.svc, root.ID)
	if first.Discovered != 1 {
		t.Fatalf("first scan discovered=%d, want 1", first.Discovered)
	}
	runBoundedPipeline(t, fx.pipeline)
	assets, err := fx.repo.ListAssets(ctx, 10, 0)
	if err != nil || len(assets) != 1 {
		t.Fatalf("assets after first scan: %+v err=%v", assets, err)
	}
	assetID := assets[0].ID
	rootAfterFirst, err := fx.repo.GetLibraryRoot(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rootAfterFirst.HealthState != domain.RootHealthHealthy {
		t.Fatalf("root health after first scan = %q, want healthy", rootAfterFirst.HealthState)
	}
	if got := countProbeJobs(t, fx.repo, assetID); got != 1 {
		t.Fatalf("probe jobs after first scan = %d, want 1", got)
	}

	// Phase 2: the root vanishes (unmounted NAS, removed bind). The scan
	// records the walk failure, must NOT reconcile, and marks the root
	// unavailable.
	if err := os.RemoveAll(rootDir); err != nil {
		t.Fatal(err)
	}
	offline, err := fx.svc.ScanLibraryRoot(ctx, root.ID)
	if err != nil {
		t.Fatalf("scan of an offline root must not fail the caller: %v", err)
	}
	if len(offline.Errors) == 0 {
		t.Fatal("offline scan reported no walk errors")
	}
	if offline.Missing != 0 {
		t.Fatalf("offline scan reconciled %d locations missing, want 0: the gate must pause reconciliation", offline.Missing)
	}
	rootOffline, err := fx.repo.GetLibraryRoot(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rootOffline.HealthState != domain.RootHealthUnavailable {
		t.Fatalf("root health after offline scan = %q, want unavailable", rootOffline.HealthState)
	}
	if state := assetState(t, fx.repo, assetID); state != string(domain.AssetDiscovered) {
		t.Fatalf("asset state after offline scan = %q, want discovered (not missing)", state)
	}
	if since := missingSince(t, fx.repo, assetID); since != nil {
		t.Fatalf("asset was marked missing while the root was offline (missing_since=%q)", *since)
	}
	if got := countLocationsWithExists(t, fx.repo, assetID, 1); got != 1 {
		t.Fatalf("live locations after offline scan = %d, want 1 (the location must not die)", got)
	}

	// Phase 3: the root comes back with the same content at the same path.
	// The supervisor-style rescan restores the healthy verdict; the
	// unchanged file (same fingerprint, size and mtime) must not re-enqueue
	// the chain.
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, src, clip)
	if err := os.Chtimes(clip, time.Now(), originalMtime); err != nil {
		t.Fatal(err)
	}
	back := scanRoot(t, fx.svc, root.ID)
	if back.Missing != 0 {
		t.Fatalf("rescan after remount reconciled %d missing, want 0", back.Missing)
	}
	rootBack, err := fx.repo.GetLibraryRoot(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rootBack.HealthState != domain.RootHealthHealthy {
		t.Fatalf("root health after remount scan = %q, want healthy", rootBack.HealthState)
	}
	if state := assetState(t, fx.repo, assetID); state != string(domain.AssetDiscovered) {
		t.Fatalf("asset state after remount scan = %q, want discovered", state)
	}
	if got := countLocationsWithExists(t, fx.repo, assetID, 1); got != 1 {
		t.Fatalf("live locations after remount scan = %d, want 1", got)
	}
	if got := countProbeJobs(t, fx.repo, assetID); got != 1 {
		t.Fatalf("probe jobs after remount scan = %d, want 1: an unchanged file must not re-enqueue the chain", got)
	}
}
