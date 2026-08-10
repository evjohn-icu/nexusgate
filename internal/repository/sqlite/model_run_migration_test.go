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

	now := formatTime(time.Now())
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
