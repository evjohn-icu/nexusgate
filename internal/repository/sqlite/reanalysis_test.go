package sqlite

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// seedReanalysisAsset creates an asset with a proxy artifact and a transcript
// so EnqueueReanalysis has something to run on.
func seedReanalysisAsset(t *testing.T, repo *Repository, suffix string) string {
	t.Helper()
	ctx := context.Background()
	assetID := "asset-reanalysis-" + suffix
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,100,'discovered',?,?)`, assetID, "fp-"+suffix, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO derived_artifacts(id,asset_id,artifact_type,profile_hash,local_path,size_bytes,created_at) VALUES(?,?,?,?,?,?,?)`, "art-"+suffix, assetID, "proxy", "p1", "/tmp/proxy-"+suffix+".mp4", 10, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO media_metadata(asset_id,ffprobe_json,exiftool_json,normalized_json,probe_version,updated_at) VALUES(?,'{}','{}',?,'test',?)`, assetID, `{"duration_ms":600000}`, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveTranscript(ctx, assetID, "qwen", "qwen3-asr-flash", "thash-"+suffix, domain.Transcript{Language: "zh", Text: "text " + suffix}); err != nil {
		t.Fatal(err)
	}
	return assetID
}

// The whole point of reanalysis: the succeeded analyze job must not swallow a
// forced re-run through the INSERT OR IGNORE input-hash dedup, and each
// invocation must produce a fresh run (nonce in the hash) while leaving the
// old one auditable. Two invocations must therefore create two distinct
// analyze jobs and two audit rows.
func TestEnqueueReanalysisBreaksDedupAndRecordsReason(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	assetID := seedReanalysisAsset(t, repo, "a")

	if err := repo.EnqueueReanalysis(ctx, assetID, "prompt v4 migration"); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueReanalysis(ctx, assetID, "schema v2 migration"); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE asset_id=? AND job_type='analyze'`, assetID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected 2 distinct analyze jobs, got %d", count)
	}
	var distinctHashes int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT input_hash) FROM jobs WHERE asset_id=? AND job_type='analyze'`, assetID).Scan(&distinctHashes); err != nil {
		t.Fatal(err)
	}
	if distinctHashes != 2 {
		t.Fatalf("reanalysis jobs must carry distinct input hashes, got %d", distinctHashes)
	}
	var requests int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM reanalysis_requests WHERE asset_id=?`, assetID).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("expected 2 audit rows, got %d", requests)
	}
	var reason string
	if err := repo.db.QueryRowContext(ctx, `SELECT reason FROM reanalysis_requests WHERE asset_id=? ORDER BY created_at DESC LIMIT 1`, assetID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "schema v2 migration" {
		t.Fatalf("audit reason = %q", reason)
	}
}

// Reanalysis on an asset with neither proxy artifact nor media metadata has
// nothing to run on; the error must say so instead of enqueueing a doomed
// job that would permanently fail at the analyze stage.
func TestEnqueueReanalysisRejectsAssetWithoutEvidence(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	assetID := seedAsset(t, repo, 9)
	if err := repo.EnqueueReanalysis(ctx, assetID, "reason"); err == nil {
		t.Fatal("reanalysis with no proxy and no transcript must fail")
	}
	var count int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE asset_id=?`, assetID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("no job must be enqueued for an asset without evidence, got %d", count)
	}
}

// The enqueued reanalysis job must be leasable and completable through the
// ordinary pipeline path — nothing about it may differ from a normal analyze —
// and the audit row must link to the job by input hash: that linkage is the
// only way to answer "which reanalysis request produced this run".
func TestEnqueueReanalysisJobFlowsThroughLease(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	assetID := seedReanalysisAsset(t, repo, "b")
	if err := repo.EnqueueReanalysis(ctx, assetID, "lease check"); err != nil {
		t.Fatal(err)
	}
	var jobHash, auditHash string
	if err := repo.db.QueryRowContext(ctx, `SELECT input_hash FROM jobs WHERE asset_id=? AND job_type='analyze'`, assetID).Scan(&jobHash); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT input_hash FROM reanalysis_requests WHERE asset_id=? ORDER BY created_at DESC LIMIT 1`, assetID).Scan(&auditHash); err != nil {
		t.Fatal(err)
	}
	if jobHash != auditHash {
		t.Fatalf("audit row does not link to the job: job=%q audit=%q", jobHash, auditHash)
	}
	job, err := repo.LeaseNextJob(ctx, "reanalysis-test", nil, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("lease reanalysis job: job=%+v err=%v", job, err)
	}
	if job.Type != domain.JobAnalyze || job.AssetID != assetID || job.InputHash != jobHash {
		t.Fatalf("wrong job leased: %+v", job)
	}
	if err := repo.CompleteJob(ctx, job.ID, "reanalysis-test", domain.JobSucceeded, ""); err != nil {
		t.Fatal(err)
	}
}

// Reanalysis must never destroy history: the old committed model run stays in
// model_runs (immutable, auditable) while a second run commits on top. This
// mirrors the real flow: run1 committed canonically, the operator forces a
// reanalysis, the new job's input hash produces run2, and CommitAnalysisWithShots
// switches the canonical rows — but run1 must still be queryable.
func TestReanalysisKeepsOldModelRunAuditable(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	assetID := seedReanalysisAsset(t, repo, "audit")

	commitRun := func(inputHash, summary string) string {
		t.Helper()
		runID, _, err := repo.CreateModelRun(ctx, assetID, "vision", "fixture", "fixture-model", inputHash, "footage-analysis-v4", "asset-analysis/v2", `{"asset_id":"`+assetID+`"}`)
		if err != nil {
			t.Fatal(err)
		}
		analysis := domain.StructuredAnalysis{Summary: summary, AssetType: "b_roll", Quality: "usable"}
		parsed, _ := json.Marshal(analysis)
		if err := repo.StageModelRun(ctx, runID, `{"raw":true}`, string(parsed)); err != nil {
			t.Fatal(err)
		}
		shots := []domain.AssetShot{{AssetID: assetID, SourceRunID: runID, Ordinal: 0, StartMS: 0, EndMS: 5_000, Description: summary}}
		if err := repo.CommitAnalysisWithShots(ctx, assetID, runID, "asset-analysis/v2", analysis, shots); err != nil {
			t.Fatal(err)
		}
		return runID
	}
	firstRun := commitRun("run-hash-1", "old analysis")

	if err := repo.EnqueueReanalysis(ctx, assetID, "prompt migration"); err != nil {
		t.Fatal(err)
	}
	var reanalysisHash string
	if err := repo.db.QueryRowContext(ctx, `SELECT input_hash FROM jobs WHERE asset_id=? AND job_type='analyze'`, assetID).Scan(&reanalysisHash); err != nil {
		t.Fatal(err)
	}
	if reanalysisHash == "run-hash-1" {
		t.Fatal("reanalysis input hash must differ from the original run's")
	}
	secondRun := commitRun(reanalysisHash, "new analysis")

	for _, id := range []string{firstRun, secondRun} {
		var state string
		if err := repo.db.QueryRowContext(ctx, `SELECT state FROM model_runs WHERE id=?`, id).Scan(&state); err != nil {
			t.Fatalf("model run %s vanished: %v", id, err)
		}
		if state != "committed" {
			t.Fatalf("model run %s state = %q, want committed", id, state)
		}
	}
	// Canonical shots point at the new run only.
	shots, err := repo.ListAssetShots(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) != 1 || shots[0].SourceRunID != secondRun {
		t.Fatalf("canonical shots did not switch to the reanalysis run: %+v", shots)
	}
}

// EnqueueRederive is the recovery half of `cache gc --rebuildable`: it puts a
// derive job back on the queue so the deleted files are re-created. Only an
// asset whose analysis already committed is eligible — re-deriving an
// in-flight chain's files would re-enqueue the paid stages (see the derive
// stage's committed check in app/pipeline.go) — and the enqueue must be a
// fresh hash every time, or the succeeded original derive would swallow it.
func TestEnqueueRederiveOnlyForCommittedAssets(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	committed := seedCommittedAsset(t, repo, "derived-committed")

	enqueued, err := repo.EnqueueRederive(ctx, committed)
	if err != nil {
		t.Fatal(err)
	}
	if !enqueued {
		t.Fatal("a committed asset must be re-derived")
	}
	var (
		count  int
		prio   int
		hashes int
	)
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(DISTINCT input_hash) FROM jobs WHERE asset_id=? AND job_type='derive'`, committed).Scan(&count, &hashes); err != nil {
		t.Fatal(err)
	}
	if count != 1 || hashes != 1 {
		t.Fatalf("expected 1 distinct derive job, got count=%d distinct_hashes=%d", count, hashes)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT priority FROM jobs WHERE asset_id=? AND job_type='derive'`, committed).Scan(&prio); err != nil {
		t.Fatal(err)
	}
	if prio != 90 {
		t.Fatalf("re-derive priority = %d, want 90", prio)
	}

	// An asset with no committed analysis must not be re-derived: its chain is
	// still in flight and a re-derive would fork a parallel paid chain.
	uncommitted := seedReanalysisAsset(t, repo, "derived-uncommitted")
	enqueued, err = repo.EnqueueRederive(ctx, uncommitted)
	if err != nil {
		t.Fatal(err)
	}
	if enqueued {
		t.Fatal("an uncommitted asset must not be re-derived")
	}
}

func TestHasCommittedAnalysis(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	committed := seedCommittedAsset(t, repo, "committed")
	got, err := repo.HasCommittedAnalysis(ctx, committed)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("asset with asset_analysis + asset_shots must be committed")
	}
	uncommitted := seedReanalysisAsset(t, repo, "uncommitted")
	got, err = repo.HasCommittedAnalysis(ctx, uncommitted)
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Fatal("asset with only metadata must not be committed")
	}
}

func TestRebuildAllSearchRepairsMissingStaleRowsAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepo(t)
	assetID := seedCommittedAsset(t, repo, "rebuild-all")
	if err := repo.RebuildSearch(ctx, assetID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `UPDATE asset_analysis SET summary='fresh rebuild term' WHERE asset_id=?`, assetID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `DELETE FROM asset_search WHERE asset_id=?`, assetID); err != nil {
		t.Fatal(err)
	}
	rebuilt, failures, err := repo.RebuildAllSearch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt != 1 || len(failures) != 0 {
		t.Fatalf("rebuild result = %d, %v; want 1 and no failures", rebuilt, failures)
	}
	if hits, err := repo.Search(ctx, "fresh rebuild term", 10); err != nil || len(hits) != 1 {
		t.Fatalf("repaired asset search = %v, err=%v", hits, err)
	}
	rebuilt, failures, err = repo.RebuildAllSearch(ctx)
	if err != nil || rebuilt != 1 || len(failures) != 0 {
		t.Fatalf("idempotent rebuild result = %d, %v, err=%v", rebuilt, failures, err)
	}
}

func TestRebuildAllSearchContinuesAfterBrokenAsset(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepo(t)
	good := seedCommittedAsset(t, repo, "rebuild-good")
	broken := seedCommittedAsset(t, repo, "rebuild-broken")
	if _, err := repo.db.ExecContext(ctx, `DELETE FROM asset_locations WHERE asset_id=?`, broken); err != nil {
		t.Fatal(err)
	}
	rebuilt, failures, err := repo.RebuildAllSearch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt != 1 || len(failures) != 1 || failures[0] == "" {
		t.Fatalf("rebuild result = %d, %v; want one success and one per-asset failure", rebuilt, failures)
	}
	if hits, err := repo.Search(ctx, "summary", 10); err != nil || len(hits) != 1 || hits[0] != good {
		t.Fatalf("good asset was not rebuilt after broken asset: hits=%v err=%v", hits, err)
	}
}

// seedCommittedAsset creates an asset that looks exactly like one whose
// analysis chain completed: primary location, media metadata, derived
// artifacts, and canonical asset_analysis + asset_shots rows (each backed by
// a model_runs row, which the foreign keys demand).
func seedCommittedAsset(t *testing.T, repo *Repository, suffix string) string {
	t.Helper()
	ctx := context.Background()
	assetID := "asset-committed-" + suffix
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,100,'ready',?,?)`, assetID, "fp-"+suffix, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES(?,?,?,?)`, "root-"+suffix, "/tmp/root-"+suffix, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO model_runs(id,asset_id,capability,provider,model,input_hash,prompt_version,schema_version,state,request_json,started_at) VALUES(?,'asset-committed-'||?,'vision','qwen','qwen-vl','run-hash-'||?,'p','s','committed','{}',?)`,
		"run-"+suffix, suffix, suffix, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,?,?,?,?,1,1,?)`,
		"loc-"+suffix, assetID, "root-"+suffix, "clip.mov", "/tmp/clip-"+suffix+".mov", 123, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO media_metadata(asset_id,ffprobe_json,exiftool_json,normalized_json,probe_version,updated_at) VALUES(?,'{}','{}',?,'test',?)`, assetID, `{"duration_ms":600000,"has_audio":true}`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_analysis(asset_id,source_run_id,schema_version,asset_type,shot_size,camera_motion,audio_type,lighting,people_count,has_speech,quality,summary,scene_tags_json,subjects_json,mood_tags_json,usable_as_json,quality_flags_json,extra_tags_json,editorial_reason,updated_at) VALUES(?,'run-'||?,'asset-analysis/v2','clip','close','static','none','day',0,0,'fine','summary','[]','[]','[]','[]','[]','[]','',?)`, assetID, suffix, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_shots(id,asset_id,source_run_id,ordinal,start_ms,end_ms,description,tags_json,objects_json,actions_json,mood_json,confidence,created_at) VALUES(?,?,'run-'||?,0,0,1000,'a committed shot','[]','[]','[]','[]',0.9,?)`, "shot-"+suffix, assetID, suffix, now); err != nil {
		t.Fatal(err)
	}
	return assetID
}
