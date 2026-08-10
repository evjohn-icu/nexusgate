package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	videoanalysis "github.com/evjohn-icu/timingdex/internal/domain/video_analysis"
	"github.com/evjohn-icu/timingdex/internal/media"
	videoproviders "github.com/evjohn-icu/timingdex/internal/providers/video"
	sqlite "github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// e2eVideoMock stands in for a vision provider. It answers deterministically
// from the metadata the pipeline passes (duration), so the same binary
// fixture produces the same committed analysis on every run: the edge
// assertions are about the chain, not about a model. The optional shots
// function lets a test script the per-asset shot structure (the multi-shot
// fixture splits at the scene change; everything else yields a single shot).
type e2eVideoMock struct {
	shots func(durationMS int64) []videoanalysis.Shot
	calls int
}

func (m *e2eVideoMock) Name() string  { return "e2e-vision-mock" }
func (m *e2eVideoMock) Model() string { return "e2e-mock-v1" }
func (m *e2eVideoMock) Capabilities() []videoproviders.Capability {
	return []videoproviders.Capability{videoproviders.CapabilityVideoAnalysis}
}

func (m *e2eVideoMock) Analyze(_ context.Context, input videoanalysis.Input) (videoanalysis.Result, string, error) {
	m.calls++
	shots := []videoanalysis.Shot{{StartMS: 0, EndMS: input.Metadata.DurationMS, Description: "scene 0"}}
	if m.shots != nil {
		shots = m.shots(input.Metadata.DurationMS)
	}
	result := videoanalysis.Result{
		Summary: "e2e mock analysis",
		Analysis: domain.StructuredAnalysis{
			Summary:         "e2e mock analysis",
			AssetType:       "b_roll",
			ShotSize:        "wide",
			CameraMotion:    "static",
			Lighting:        "unknown",
			AudioType:       "silence",
			Quality:         "usable",
			SceneTags:       []string{"e2e-fixture"},
			EditorialReason: "Deterministic e2e mock provider: the chain, not the model, is under test.",
		},
		Shots: shots,
	}
	raw, _ := json.Marshal(result)
	return result, string(raw), nil
}

// generateSceneChangeClip concatenates two lavfi segments (testsrc then
// testsrc2, say) into one clip, so a single file carries a visual scene
// change mid-timeline — the multi-shot family. Both segments render through
// generateClip, so the suite's skip contract (skips name the missing binary
// or the encoder error) is inherited. The concat demuxer is used rather than
// a filter graph: the segments share codec parameters, and `-c copy` keeps
// the result byte-identical to the segments, with no filter-render
// differences between the halves.
func generateSceneChangeClip(t testing.TB, outDir, name string, first, second ClipOpts) string {
	t.Helper()
	scratch := t.TempDir()
	firstPath := generateClip(t, scratch, "first.mp4", first)
	secondPath := generateClip(t, scratch, "second.mp4", second)
	list := filepath.Join(scratch, "list.txt")
	if err := os.WriteFile(list, []byte(fmt.Sprintf("file '%s'\nfile '%s'\n", firstPath, secondPath)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(name, ".mp4") {
		name += ".mp4"
	}
	out := filepath.Join(outDir, name)
	cmd := exec.CommandContext(context.Background(), "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "concat", "-safe", "0", "-i", list, "-c", "copy", out)
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("scene-change fixture cannot be concatenated by local ffmpeg: %v: %s", err, truncateStderr(raw))
	}
	// The same verify-before-chain contract as generateClip: a clip that
	// cannot probe would fail the pipeline's probe stage with an error that
	// looks like a pipeline bug.
	if _, err := media.Probe(context.Background(), out); err != nil {
		t.Skipf("generated scene-change fixture %s does not probe cleanly: %v", name, err)
	}
	return out
}

// openE2ERepo opens a fresh SQLite database under a temp data dir and runs
// the migrations, exactly as the Hub does on first start.
func openE2ERepo(t *testing.T) (*sqlite.Repository, string) {
	t.Helper()
	dataDir := t.TempDir()
	repo, err := sqlite.Open(filepath.Join(dataDir, "timingdex.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return repo, dataDir
}

// newE2EService wires the runnable half of the edge tests in the same shape
// as the core suite's chainEnv: a real app.Service (built by the public
// constructor, so the scan boundary is the real one) whose own pipeline is
// never run, plus a separate pipeline wired with the mock vision provider —
// the chain runs there, leasing the same jobs the scan enqueued. ASR,
// alignment and the shot detector are nil: the edge fixtures are no-audio,
// so the chain never reaches those stages.
func newE2EService(t *testing.T, repo *sqlite.Repository, dataDir string, video videoproviders.VideoUnderstandingProvider) (*app.Service, *app.Pipeline) {
	t.Helper()
	cacheDir := filepath.Join(dataDir, "cache")
	service, err := app.NewService(repo, config.Config{DataDir: dataDir, CacheDir: cacheDir, Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	pipeline := app.NewPipeline(repo, cacheDir, nil, nil, video, nil, nil,
		media.HardwarePlan{Mode: "software", BitrateKbps: 1800}, nil, 0, 0)
	return service, pipeline
}

// runBoundedPipeline runs the mock-wired pipeline under a context deadline.
// The deadline is the guard against the failure this suite exists for: a
// stage that never idles would otherwise hang the test runner instead of
// failing a test. RunUntilIdle returning nil is both "the chain completed"
// and "it completed before the deadline".
func runBoundedPipeline(t *testing.T, pipeline *app.Pipeline) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := pipeline.RunUntilIdle(ctx); err != nil {
		t.Fatalf("bounded pipeline run failed: %v", err)
	}
}

// scanRoot runs one scan pass and fails the test on a scan error. A scan of
// a missing root is not an error (it returns the unavailable verdict), so
// this helper is only used where the root is expected to be reachable.
func scanRoot(t *testing.T, svc *app.Service, rootID string) domain.ScanResult {
	t.Helper()
	result, err := svc.ScanLibraryRoot(context.Background(), rootID)
	if err != nil {
		t.Fatalf("scan %s: %v", rootID, err)
	}
	return result
}

// copyFile copies src to dst. A real copy, not a rename: the multi-location
// fixture needs the same bytes at two paths, and the root-offline fixture
// must re-create a file that was removed.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// countModelRuns is the paid-work counter: every provider call the pipeline
// makes is recorded as a model_runs row, so the multi-location test can
// prove the same content was not analysed twice.
func countModelRuns(t *testing.T, repo *sqlite.Repository, assetID string) int {
	t.Helper()
	var count int
	if err := repo.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM model_runs WHERE asset_id=?`, assetID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// countLocationsWithExists counts the asset's asset_locations rows by their
// exists_now flag (1 = live, 0 = reconciled missing).
func countLocationsWithExists(t *testing.T, repo *sqlite.Repository, assetID string, exists int) int {
	t.Helper()
	var count int
	if err := repo.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM asset_locations WHERE asset_id=? AND exists_now=?`, assetID, exists).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// countProbeJobs counts the asset's probe jobs, the dedup canary for
// re-enqueues: a scan pass that re-enqueues the chain would mint a second
// probe row.
func countProbeJobs(t *testing.T, repo *sqlite.Repository, assetID string) int {
	t.Helper()
	var count int
	if err := repo.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM jobs WHERE asset_id=? AND job_type='probe'`, assetID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func assetState(t *testing.T, repo *sqlite.Repository, assetID string) string {
	t.Helper()
	var state string
	if err := repo.DB().QueryRowContext(context.Background(), `SELECT state FROM assets WHERE id=?`, assetID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func missingSince(t *testing.T, repo *sqlite.Repository, assetID string) *string {
	t.Helper()
	var v sql.NullString
	if err := repo.DB().QueryRowContext(context.Background(), `SELECT missing_since FROM assets WHERE id=?`, assetID).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if !v.Valid {
		return nil
	}
	return &v.String
}
