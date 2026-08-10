package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// TestModelRunMigration0031UpgradeAndForeignKeys applies every shipped
// migration before 0031, then applies 0031 through Migrate. This keeps the
// test on the same upgrade path as an existing 0029 database.
func TestModelRunMigration0031UpgradeAndForeignKeys(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "pre-0031.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	if _, err := repo.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "0031_model_run_retryable_dedup.sql" {
			continue
		}
		content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.ExecContext(ctx, string(content)); err != nil {
			t.Fatalf("apply %s: %v", entry.Name(), err)
		}
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(?,?)`, entry.Name(), formatTime(time.Now())); err != nil {
			t.Fatal(err)
		}
	}
	now := formatTime(time.Now())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('populated-root','/populated',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('populated-asset','populated-fingerprint',42,'analyzed',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO model_runs(id,asset_id,capability,provider,model,input_hash,prompt_version,schema_version,state,request_json,raw_response,parsed_json,validation_errors,error_code,error_message,token_input,token_output,started_at,finished_at,committed_at) VALUES('populated-run','populated-asset','vision','provider','model','populated-hash','p1','s1','committed','{"request":true}','{"raw":true}','{"parsed":true}',NULL,NULL,NULL,11,22,?,?,?)`, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_analysis(asset_id,source_run_id,schema_version,asset_type,shot_size,camera_motion,audio_type,lighting,people_count,has_speech,quality,summary,scene_tags_json,subjects_json,mood_tags_json,usable_as_json,quality_flags_json,extra_tags_json,editorial_reason,updated_at) VALUES('populated-asset','populated-run','s1','b-roll','wide','static','none','daylight',2,0,'good','preserved summary','["scene"]','["subject"]','["mood"]','["use"]','["quality"]','["extra"]','reason',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_shots(id,asset_id,source_run_id,ordinal,start_ms,end_ms,description,tags_json,objects_json,actions_json,mood_json,confidence,created_at) VALUES('populated-shot','populated-asset','populated-run',3,100,900,'preserved shot','["tag"]','["object"]','["action"]','["mood"]',0.75,?)`, now); err != nil {
		t.Fatal(err)
	}

	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var violations int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if violations != 0 {
		t.Fatalf("foreign key violations after 0031 migration: %d", violations)
	}

	var runState, requestJSON, rawResponse, parsedJSON string
	var tokenInput, tokenOutput int
	if err := repo.db.QueryRowContext(ctx, `SELECT state,request_json,raw_response,parsed_json,token_input,token_output FROM model_runs WHERE id='populated-run'`).Scan(&runState, &requestJSON, &rawResponse, &parsedJSON, &tokenInput, &tokenOutput); err != nil {
		t.Fatal(err)
	}
	if runState != "committed" || requestJSON != `{"request":true}` || rawResponse != `{"raw":true}` || parsedJSON != `{"parsed":true}` || tokenInput != 11 || tokenOutput != 22 {
		t.Fatalf("populated model run changed: state=%s request=%s raw=%s parsed=%s tokens=%d/%d", runState, requestJSON, rawResponse, parsedJSON, tokenInput, tokenOutput)
	}
	var analysisSummary, shotDescription string
	var shotRunID, analysisRunID string
	if err := repo.db.QueryRowContext(ctx, `SELECT source_run_id,summary FROM asset_analysis WHERE asset_id='populated-asset'`).Scan(&analysisRunID, &analysisSummary); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT source_run_id,description FROM asset_shots WHERE id='populated-shot'`).Scan(&shotRunID, &shotDescription); err != nil {
		t.Fatal(err)
	}
	if analysisRunID != "populated-run" || analysisSummary != "preserved summary" || shotRunID != "populated-run" || shotDescription != "preserved shot" {
		t.Fatalf("populated relationships changed: analysis=%s/%s shot=%s/%s", analysisRunID, analysisSummary, shotRunID, shotDescription)
	}

	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('migration-root','/migration',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('migration-asset','fingerprint',1,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	runID, _, err := repo.CreateModelRun(ctx, "migration-asset", "vision", "provider", "model", "migration-hash", "p1", "s1", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.StageModelRun(ctx, runID, `{}`, `{}`); err != nil {
		t.Fatal(err)
	}
	if err := repo.CommitAnalysis(ctx, "migration-asset", runID, "s1", domain.StructuredAnalysis{AssetType: "b-roll", Summary: "migration"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `DELETE FROM model_runs WHERE id=?`, runID); err != nil {
		t.Fatal(err)
	}
	var sourceRunID *string
	if err := repo.db.QueryRowContext(ctx, `SELECT source_run_id FROM asset_analysis WHERE asset_id='migration-asset'`).Scan(&sourceRunID); err != nil {
		t.Fatal(err)
	}
	if sourceRunID != nil {
		t.Fatalf("asset_analysis source_run_id = %q, want NULL", *sourceRunID)
	}

	secondRun, _, err := repo.CreateModelRun(ctx, "migration-asset", "vision", "provider", "model", "migration-hash", "p1", "s1", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if secondRun == runID {
		t.Fatal("retry after failed/deleted run reused the old ID")
	}
	if err := repo.FailModelRun(ctx, secondRun, "retry", "failed", `{}`); err != nil {
		t.Fatal(err)
	}
	thirdRun, _, err := repo.CreateModelRun(ctx, "migration-asset", "vision", "provider", "model", "migration-hash", "p1", "s1", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if thirdRun == secondRun {
		t.Fatal("retry after failed run reused the failed ID")
	}

	if _, err := repo.db.ExecContext(ctx, `DELETE FROM assets WHERE id='migration-asset'`); err != nil {
		t.Fatal(err)
	}
	var remainingRuns, remainingAnalysis int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM model_runs WHERE asset_id='migration-asset'`).Scan(&remainingRuns); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_analysis WHERE asset_id='migration-asset'`).Scan(&remainingAnalysis); err != nil {
		t.Fatal(err)
	}
	if remainingRuns != 0 || remainingAnalysis != 0 {
		t.Fatalf("asset cascade left runs=%d analysis=%d", remainingRuns, remainingAnalysis)
	}
}
