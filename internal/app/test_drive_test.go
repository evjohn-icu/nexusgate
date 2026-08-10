package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// openTestDriveHub builds a migrated real-SQLite Hub with one scanned asset
// per id and a Service wired the way the rest of the package constructs one.
// The claims under test — "TestDrive enqueues this asset", "an asset with
// shots is skipped" — are claims about what the database accepts, so the
// queue rows and shot rows must be real rows, exactly as pipeline_defer_test
// argues for the queue.
func openTestDriveHub(t *testing.T, ids ...string) (*sqlite.Repository, *Service, []string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	repo, err := sqlite.Open(filepath.Join(dir, "test-drive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rootDir := filepath.Join(dir, "footage")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateLibraryRoot(ctx, rootDir)
	if err != nil {
		t.Fatal(err)
	}
	assetIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		source := filepath.Join(rootDir, id+".mov")
		if err := os.WriteFile(source, []byte("footage "+id), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(source)
		if err != nil {
			t.Fatal(err)
		}
		scanned, err := repo.UpsertScannedFile(ctx, root, id+".mov", source, info, "fp-"+id)
		if err != nil {
			t.Fatal(err)
		}
		assetIDs = append(assetIDs, scanned.AssetID)
	}
	service, err := NewService(repo, config.Config{DataDir: dir, CacheDir: filepath.Join(dir, "cache"), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	return repo, service, assetIDs
}

// seedTestDriveShots commits canonical shot rows the way an analyze run would,
// so the already-analyzed skip and the suggestion aggregation see real rows.
// The source run is empty — asset_shots.source_run_id is a foreign key to
// model_runs, and the seed exists for its shots, not for a run.
func seedTestDriveShots(t *testing.T, repo *sqlite.Repository, assetID string, shots []domain.AssetShot) {
	t.Helper()
	if err := repo.ReplaceAssetShots(context.Background(), assetID, "", shots); err != nil {
		t.Fatal(err)
	}
}

// TestTestDriveEnqueuesAssetWithoutShots pins the happy path: an asset with
// no committed analysis gets a probe job and the pass runs once.
func TestTestDriveEnqueuesAssetWithoutShots(t *testing.T) {
	repo, service, ids := openTestDriveHub(t, "clip-a")
	result, err := service.TestDrive(context.Background(), []string{ids[0]})
	if err != nil {
		t.Fatal(err)
	}
	if result.Enqueued != 1 || result.AlreadyAnalyzed != 0 || !result.Ran {
		t.Fatalf("result=%+v, want enqueued=1 already_analyzed=0 ran=true", result)
	}
	jobs, err := repo.ListJobs(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Type != domain.JobProbe || jobs[0].AssetID != ids[0] {
		t.Fatalf("expected one probe job for the asset, jobs=%+v", jobs)
	}
}

// TestTestDriveSkipsAssetWithCommittedShots pins the idempotence: an asset
// whose shots are already committed is counted, not re-enqueued, and the
// queue stays empty.
func TestTestDriveSkipsAssetWithCommittedShots(t *testing.T) {
	repo, service, ids := openTestDriveHub(t, "clip-b")
	ctx := context.Background()
	seedTestDriveShots(t, repo, ids[0], []domain.AssetShot{
		{Ordinal: 0, StartMS: 0, EndMS: 5000, Description: "a person walking", Objects: []string{"person"}, Actions: []string{"walking"}},
	})
	result, err := service.TestDrive(ctx, []string{ids[0]})
	if err != nil {
		t.Fatal(err)
	}
	if result.Enqueued != 0 || result.AlreadyAnalyzed != 1 || !result.Ran {
		t.Fatalf("result=%+v, want enqueued=0 already_analyzed=1 ran=true", result)
	}
	jobs, err := repo.ListJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("an analyzed asset must not be enqueued, jobs=%+v", jobs)
	}
}

// TestTestDriveRejectsInvalidBatches pins the input contract: an empty batch,
// a batch over the three-asset cap, and a batch naming a missing asset are
// all the one invalid-request identity, so the API can answer them all 400.
func TestTestDriveRejectsInvalidBatches(t *testing.T) {
	_, service, ids := openTestDriveHub(t, "clip-c")
	ctx := context.Background()
	if _, err := service.TestDrive(ctx, nil); err == nil || !errors.Is(err, ErrTestDriveInvalid) {
		t.Fatalf("an empty batch must be rejected as %v, got %v", ErrTestDriveInvalid, err)
	}
	if _, err := service.TestDrive(ctx, []string{"a", "b", "c", "d"}); err == nil || !errors.Is(err, ErrTestDriveInvalid) {
		t.Fatalf("a four-asset batch must be rejected as %v, got %v", ErrTestDriveInvalid, err)
	}
	if _, err := service.TestDrive(ctx, []string{ids[0], "no-such-asset"}); err == nil || !errors.Is(err, ErrTestDriveInvalid) {
		t.Fatalf("a missing asset must be rejected as %v, got %v", ErrTestDriveInvalid, err)
	}
}

// TestTestDriveSuggestionsAggregateTopTerms pins the aggregation contract:
// objects, actions and tags across committed shots are counted per shot, and
// the result is deterministic — count descending, term ascending — with the
// default limit applied when the caller passes zero.
func TestTestDriveSuggestionsAggregateTopTerms(t *testing.T) {
	repo, service, ids := openTestDriveHub(t, "clip-d", "clip-e")
	ctx := context.Background()
	seedTestDriveShots(t, repo, ids[0], []domain.AssetShot{
		{Ordinal: 0, StartMS: 0, EndMS: 5000, Description: "person on a bike at sunset", Objects: []string{"person", "bike"}, Actions: []string{"walking"}, Tags: []string{"sunset"}},
		{Ordinal: 1, StartMS: 5000, EndMS: 10000, Description: "person running in the city", Objects: []string{"person"}, Actions: []string{"running"}, Tags: []string{"urban"}},
	})
	seedTestDriveShots(t, repo, ids[1], []domain.AssetShot{
		{Ordinal: 0, StartMS: 0, EndMS: 5000, Description: "bike ride at sunset", Objects: []string{"bike"}, Actions: []string{"walking"}, Tags: []string{"sunset"}},
	})

	got, err := service.TestDriveSuggestions(ctx, ids, 0)
	if err != nil {
		t.Fatal(err)
	}
	// bike/person/sunset/walking tie at 2 (term asc), running leads the 1s.
	if want := []string{"bike", "person", "sunset", "walking", "running"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("suggestions=%v, want %v", got, want)
	}

	got, err = service.TestDriveSuggestions(ctx, ids, 3)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"bike", "person", "sunset"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("limit 3 suggestions=%v, want %v", got, want)
	}

	got, err = service.TestDriveSuggestions(ctx, []string{ids[0], "no-such-asset"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// asset d alone: person appears in both shots; the other five terms tie at
	// 1 and sort alphabetically.
	if want := []string{"person", "bike", "running", "sunset", "urban"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("a missing asset must not change the aggregation, got %v want %v", got, want)
	}
}
