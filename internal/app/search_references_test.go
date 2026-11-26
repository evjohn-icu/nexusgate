package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/remote"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
)

// newSearchReferenceService builds a real SQLite-backed service with its
// managed cache under the temporary data directory, so purge tests can assert
// on both the rows and the physical files.
func newSearchReferenceService(t *testing.T) (*Service, *sqlite.Repository) {
	t.Helper()
	dir := secureDataDir(t)
	repo, err := sqlite.Open(filepath.Join(dir, "search-references.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repo, config.Config{DataDir: dir, CacheDir: filepath.Join(dir, "cache")})
	if err != nil {
		t.Fatal(err)
	}
	return service, repo
}

// seedReferenceRow writes a reference row and its managed bytes directly, so
// the test controls the lifetime instead of waiting on a real TTL.
func seedReferenceRow(t *testing.T, service *Service, repo *sqlite.Repository, id string, created, expires time.Time, withFile bool) domain.SearchReference {
	t.Helper()
	ctx := context.Background()
	contents := []byte("jpeg-bytes-" + id)
	digest := sha256.Sum256(contents)
	relative := filepath.Join("search-references", id+".jpg")
	absolute := filepath.Join(service.CacheDir(), relative)
	if withFile {
		if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ref := domain.SearchReference{
		ID:                id,
		ContentHash:       "sha256:" + hex.EncodeToString(digest[:]),
		MediaType:         "image/jpeg",
		RelativeCachePath: relative,
		Width:             2,
		Height:            2,
		CreatedAt:         created,
		ExpiresAt:         expires,
	}
	if err := repo.CreateSearchReference(ctx, ref); err != nil {
		t.Fatalf("seed reference %s: %v", id, err)
	}
	return ref
}

// seedQueryTask enqueues a real image-input inference task bound to the
// reference, the way CreateSearchReference's enqueue does. modelRevision lets
// a test keep unrelated tasks out of the capability-filtered lease.
func seedQueryTask(t *testing.T, repo *sqlite.Repository, referenceID, modelRevision string) string {
	t.Helper()
	ref, err := repo.GetSearchReference(context.Background(), referenceID)
	if err != nil {
		t.Fatal(err)
	}
	request := domain.InferenceRequest{
		SchemaVersion: domain.InferenceSchemaVersion,
		OperationID:   "operation-query-" + referenceID,
		Operation:     domain.OpVisualEmbedding,
		ModelID:       "query-model",
		ModelRevision: modelRevision,
		SpaceID:       "space-query",
		InputHash:     "input-" + referenceID,
		ConfigHash:    "config-query",
		TimeoutMS:     60_000,
		Input:         domain.InferenceInput{Kind: domain.InputKindImage, ReferenceID: referenceID, Digest: ref.ContentHash},
		Visual:        &domain.VisualSettings{},
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	task := domain.InferenceTask{
		TaskKey:       "query-" + referenceID,
		Operation:     domain.OpVisualEmbedding,
		RequestJSON:   string(encoded),
		ConfigHash:    request.ConfigHash,
		ModelRevision: request.ModelRevision,
	}
	jobID, err := repo.EnqueueInferenceTask(context.Background(), nil, task, 60)
	if err != nil {
		t.Fatalf("seed query task for %s: %v", referenceID, err)
	}
	return jobID
}

func queryTaskFlagged(t *testing.T, repo *sqlite.Repository, jobID string) bool {
	t.Helper()
	task, err := repo.GetInferenceTask(context.Background(), jobID)
	if err != nil {
		t.Fatalf("read task %s: %v", jobID, err)
	}
	return task.CancelRequested
}

func queryCapability() []remote.InferenceCapability {
	return []remote.InferenceCapability{{
		Operation:     domain.OpVisualEmbedding,
		ModelID:       "query-model",
		ModelRevision: "revision-1",
		ConfigHash:    "config-query",
	}}
}

// Purging expired references must cancel the bound work before touching the
// recorded bytes, remove the row only after the file is gone, retain the row
// for retry when removal fails, treat a missing file as success, and leave
// unexpired references completely alone. A cancelled job stays terminal: heal
// never revives it, its renewal is denied, and no later lease hands it out.
func TestPurgeExpiredSearchReferencesRemovalPolicy(t *testing.T) {
	ctx := context.Background()
	service, repo := newSearchReferenceService(t)
	now := time.Now().UTC()

	// Only the expired reference's task matches the lease capability, so the
	// lease assertions below are unambiguous.
	expired := seedReferenceRow(t, service, repo, "ref-expired", now.Add(-time.Hour), now.Add(15*time.Minute), true)
	missing := seedReferenceRow(t, service, repo, "ref-missing-file", now.Add(-time.Hour), now.Add(15*time.Minute), false)
	live := seedReferenceRow(t, service, repo, "ref-live", now.Add(-time.Hour), now.Add(15*time.Minute), true)
	expiredJob := seedQueryTask(t, repo, expired.ID, "revision-1")
	seedQueryTask(t, repo, "ref-missing-file", "revision-2")
	liveJob := seedQueryTask(t, repo, live.ID, "revision-2")

	// The expired job is leasable before the purge: the cancellation below
	// takes live work with the reference, not an idle queue. The lease clock
	// sits a minute past the enqueue stamps so run_after is always due.
	job, err := repo.LeaseNextLocalInference(ctx, "local-test", time.Hour, queryCapability(), 0, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if job == nil || job.JobID != expiredJob {
		t.Fatalf("first lease = %+v, want the expired reference's pending job %s", job, expiredJob)
	}
	if _, err := repo.DB().ExecContext(ctx, `UPDATE search_references SET expires_at=? WHERE id IN (?,?)`, now.Add(-time.Minute).UTC().Format("2006-01-02T15:04:05.000000000Z"), expired.ID, missing.ID); err != nil {
		t.Fatal(err)
	}

	// Make the recorded bytes undeletable: the managed path now holds a
	// non-empty directory, so os.Remove fails on every platform and account.
	managedPath := filepath.Join(service.CacheDir(), expired.RelativeCachePath)
	if err := os.RemoveAll(managedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(managedPath, "held"), 0o700); err != nil {
		t.Fatal(err)
	}

	removed, err := service.PurgeExpiredSearchReferences(ctx)
	if err == nil {
		t.Fatal("purge reported success despite the undeletable file")
	}
	if removed != 1 {
		t.Fatalf("purge removed=%d, want 1 (only the fileless reference could complete)", removed)
	}
	// The bound work was cancelled before the failed removal: the running
	// lease is refused a renewal and the task carries the cancellation flag.
	if !queryTaskFlagged(t, repo, expiredJob) {
		t.Fatal("expired reference's task was not flagged cancel_requested before the failed removal")
	}
	if err := repo.RenewInferenceLease(ctx, expiredJob, "local-test", now.Add(time.Hour)); err == nil {
		t.Fatal("renewal succeeded for a cancelled job, want denied")
	}
	// The row is retained for retry: it still lists as expired, and its bytes
	// are still on disk.
	kept, err := repo.ExpiredSearchReferences(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].ID != "ref-expired" {
		t.Fatalf("retained expired rows = %+v, want only ref-expired", kept)
	}
	if _, err := os.Stat(managedPath); err != nil {
		t.Fatalf("managed bytes vanished despite the failed removal: %v", err)
	}
	// The unexpired reference is untouched: row readable, bytes present, task
	// live.
	if _, absolute, err := service.GetSearchReference(ctx, "ref-live"); err != nil {
		t.Fatalf("unexpired reference became unreadable: %v", err)
	} else if _, err := os.Stat(absolute); err != nil {
		t.Fatalf("unexpired reference bytes missing: %v", err)
	}
	if queryTaskFlagged(t, repo, liveJob) {
		t.Fatal("unexpired reference's task was cancelled")
	}

	// Healing and heartbeats must never revive the cancelled job, and no
	// later lease may hand it out again.
	released, terminal, err := repo.HealStaleRunningJobs(ctx, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if released != 0 || terminal != 0 {
		t.Fatalf("heal released=%d terminal=%d after cancellation, want 0/0", released, terminal)
	}
	if err := repo.RenewInferenceLease(ctx, expiredJob, "local-test", now.Add(2*time.Hour)); err == nil {
		t.Fatal("renewal succeeded after heal, want denied")
	}
	if job, err := repo.LeaseNextLocalInference(ctx, "local-test", time.Hour, queryCapability(), 0, now.Add(2*time.Minute)); err != nil || job != nil {
		t.Fatalf("lease after cancellation+heal = %+v err=%v, want no lease", job, err)
	}

	// Retry: once the bytes are removable the next purge completes the row's
	// removal; a now-missing file is success.
	if err := os.RemoveAll(managedPath); err != nil {
		t.Fatal(err)
	}
	removed, err = service.PurgeExpiredSearchReferences(ctx)
	if err != nil {
		t.Fatalf("retry purge failed: %v", err)
	}
	if removed != 1 {
		t.Fatalf("retry purge removed=%d, want 1", removed)
	}
	if _, err := os.Stat(managedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed path after retry = %v, want gone", err)
	}
	remaining, err := repo.ExpiredSearchReferences(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expired rows after retry = %+v, want none", remaining)
	}
	if _, _, err := service.GetSearchReference(ctx, "ref-live"); err != nil {
		t.Fatalf("unexpired reference lost by the retries: %v", err)
	}
	if _, err := os.Stat(filepath.Join(service.CacheDir(), live.RelativeCachePath)); err != nil {
		t.Fatalf("unexpired reference bytes lost by the retries: %v", err)
	}
}

// Explicit deletion follows the same policy: cancel first, bytes before row,
// missing bytes are success, and an unknown id is not found.
func TestDeleteSearchReferenceRemovalPolicy(t *testing.T) {
	ctx := context.Background()
	service, repo := newSearchReferenceService(t)
	now := time.Now().UTC()

	present := seedReferenceRow(t, service, repo, "ref-del", now.Add(-time.Hour), now.Add(15*time.Minute), true)
	jobID := seedQueryTask(t, repo, "ref-del", "revision-1")
	seedReferenceRow(t, service, repo, "ref-nofile", now.Add(-time.Hour), now.Add(15*time.Minute), false)

	if err := service.DeleteSearchReference(ctx, "ref-del"); err != nil {
		t.Fatalf("delete existing reference: %v", err)
	}
	if _, err := os.Stat(filepath.Join(service.CacheDir(), present.RelativeCachePath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted reference bytes = %v, want gone", err)
	}
	if _, _, err := service.GetSearchReference(ctx, "ref-del"); !errors.Is(err, ErrSearchReferenceNotFound) {
		t.Fatalf("deleted reference read = %v, want not found", err)
	}
	if !queryTaskFlagged(t, repo, jobID) {
		t.Fatal("deletion did not cancel the bound task")
	}
	if err := repo.RenewInferenceLease(ctx, jobID, "local-any", now.Add(time.Hour)); err == nil {
		t.Fatal("renewal succeeded for a deleted reference's job, want denied")
	}

	// A missing file does not block deletion: the row is the authority.
	if err := service.DeleteSearchReference(ctx, "ref-nofile"); err != nil {
		t.Fatalf("delete reference with missing bytes: %v", err)
	}
	if _, _, err := service.GetSearchReference(ctx, "ref-nofile"); !errors.Is(err, ErrSearchReferenceNotFound) {
		t.Fatalf("fileless reference read after delete = %v, want not found", err)
	}

	// An unknown id is an error, not a silent success.
	if err := service.DeleteSearchReference(ctx, "ref-ghost"); !errors.Is(err, ErrSearchReferenceNotFound) {
		t.Fatalf("delete unknown reference = %v, want not found", err)
	}
}
