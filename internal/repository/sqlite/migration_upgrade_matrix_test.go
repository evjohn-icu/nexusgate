package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func applyMigrationsThrough(t *testing.T, ctx context.Context, repo *Repository, through string) {
	t.Helper()
	if _, err := repo.db.ExecContext(ctx, `CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() > through {
			continue
		}
		content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.ExecContext(ctx, string(content)); err != nil {
			t.Fatalf("apply %s: %v", entry.Name(), err)
		}
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`, entry.Name(), formatTime(time.Now().UTC())); err != nil {
			t.Fatal(err)
		}
	}
}

func populateDirtyUpgradeCorpus(t *testing.T, ctx context.Context, repo *Repository, withCostLedger bool) {
	t.Helper()
	now := "2000-01-01T00:00:00.000000000Z"
	if withCostLedger {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at,health_state) VALUES
			('matrix-root-a','/matrix/a',?,?,'healthy'),
			('matrix-root-b','/matrix/b',?,?,'unknown'),
			('matrix-root-c','/matrix/c',?,?,'unavailable')`, now, now, now, now, now, now); err != nil {
			t.Fatal(err)
		}
	} else if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES
		('matrix-root-a','/matrix/a',?,?), ('matrix-root-b','/matrix/b',?,?), ('matrix-root-c','/matrix/c',?,?)`, now, now, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 48; i++ {
		assetID := fmt.Sprintf("matrix-asset-%02d", i)
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,?,?,?,?)`, assetID, "matrix-fp-"+assetID, 100+i, "analyzed", now, now); err != nil {
			t.Fatal(err)
		}
		for j, root := range []string{"matrix-root-a", "matrix-root-b", "matrix-root-c"} {
			locationID := fmt.Sprintf("matrix-location-%02d-%d", i, j)
			seen := fmt.Sprintf("2000-01-%02dT00:00:00.000000000Z", j+1)
			exists := j != 2
			if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,?,?,?,?,?,?,?)`, locationID, assetID, root, fmt.Sprintf("clip-%02d-%d.mp4", i, j), "/matrix", 1000+i*10+j, exists, 1, seen); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO transcripts(id,asset_id,provider,model,input_hash,language,full_text,segments_json,raw_response,status,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "matrix-transcript-"+assetID, assetID, "fixture", "fixture", "transcript-"+assetID, "en", "preserved transcript", "[]", "{}", "committed", now); err != nil {
			t.Fatal(err)
		}
		for _, state := range []string{"failed", "running", "committed"} {
			runID := fmt.Sprintf("matrix-run-%02d-%s", i, state)
			if _, err := repo.db.ExecContext(ctx, `INSERT INTO model_runs(id,asset_id,capability,provider,model,input_hash,prompt_version,schema_version,state,request_json,raw_response,parsed_json,validation_errors,error_code,error_message,token_input,token_output,started_at,finished_at,committed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, runID, assetID, "vision", "fixture", "model", "matrix-"+assetID+"-"+state, "p1", "s1", state, `{}`, `{}`, `{}`, "", "fixture", "preserved", 1, 2, now, now, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_collections(id,name,description,filter_json,created_at,updated_at) VALUES('matrix-collection','Matrix','','{}',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 24; i++ {
		shotID := fmt.Sprintf("matrix-shot-%02d", i)
		assetID := fmt.Sprintf("matrix-asset-%02d", i)
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_shots(id,asset_id,source_run_id,ordinal,start_ms,end_ms,description,tags_json,objects_json,actions_json,mood_json,confidence,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, shotID, assetID, fmt.Sprintf("matrix-run-%02d-committed", i), i, i*1000, i*1000+500, "preserved shot", "[]", "[]", "[]", "[]", 0.5, now); err != nil {
			t.Fatal(err)
		}
		if withCostLedger {
			if _, err := repo.db.ExecContext(ctx, `INSERT INTO collection_shots(collection_id,shot_id,position,created_at) VALUES(?,?,?,?)`, "matrix-collection", shotID, i%4, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	if withCostLedger {
		for i := 0; i < 48; i++ {
			if _, err := repo.db.ExecContext(ctx, `INSERT INTO cost_ledger(id,day,capability,provider,model,asset_id,estimate,created_at) VALUES(?,?,?,?,?,?,?,?)`, fmt.Sprintf("matrix-cost-%02d", i), "2000-01-01", "vision", "fixture", "model", fmt.Sprintf("matrix-asset-%02d", i), 0.25, now); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func assertUpgradeMatrix(t *testing.T, ctx context.Context, repo *Repository) {
	t.Helper()
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var expected int
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".sql" {
			expected++
		}
	}
	var applied, duplicates int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT version FROM schema_migrations GROUP BY version HAVING COUNT(*) > 1)`).Scan(&duplicates); err != nil {
		t.Fatal(err)
	}
	if applied != expected || duplicates != 0 {
		t.Fatalf("schema migrations applied=%d want=%d duplicates=%d", applied, expected, duplicates)
	}
	var badPrimaries, badPositions, violations int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT asset_id FROM asset_locations WHERE is_primary=1 GROUP BY asset_id HAVING COUNT(*) != 1)`).Scan(&badPrimaries); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT collection_id FROM collection_shots GROUP BY collection_id HAVING MIN(position) != 0 OR MAX(position) != COUNT(*)-1 OR COUNT(DISTINCT position) != COUNT(*))`).Scan(&badPositions); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if badPrimaries != 0 || badPositions != 0 || violations != 0 {
		t.Fatalf("post-upgrade invariants primaries=%d positions=%d foreign_keys=%d", badPrimaries, badPositions, violations)
	}
	var probe string
	if err := repo.db.QueryRowContext(ctx, `SELECT probe_modified_ns FROM assets WHERE id='matrix-asset-00'`).Scan(&probe); err != nil {
		t.Fatal(err)
	}
	if probe != "1000" {
		t.Fatalf("probe_modified_ns=%q, want earliest live location 1000", probe)
	}
	var failed int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM model_runs WHERE state='failed'`).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 48 {
		t.Fatalf("failed model runs=%d, want 48 preserved", failed)
	}
}

func TestMigrationUpgradeMatrixFrom0021And0029(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cutoff string
		ledger bool
	}{
		{"0021", "0021_v021_sortable_timestamps.sql", false},
		{"0029", "0029_cost_ledger.sql", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			repo, err := Open(filepath.Join(t.TempDir(), "matrix.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer repo.Close()
			applyMigrationsThrough(t, ctx, repo, tc.cutoff)
			populateDirtyUpgradeCorpus(t, ctx, repo, tc.ledger)
			if err := repo.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			assertUpgradeMatrix(t, ctx, repo)
			var failedID string
			if err := repo.db.QueryRowContext(ctx, `SELECT id FROM model_runs WHERE asset_id='matrix-asset-00' AND state='failed'`).Scan(&failedID); err != nil {
				t.Fatal(err)
			}
			retryID, _, err := repo.CreateModelRun(ctx, "matrix-asset-00", "vision", "fixture", "model", "matrix-matrix-asset-00-failed", "p1", "s1", `{}`, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if retryID == failedID {
				t.Fatalf("retry reused failed model run id %q", retryID)
			}
			if err := repo.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			assertUpgradeMatrix(t, ctx, repo)
		})
	}
}

func TestMigrationUpgradeSnapshotRestoreFrom0029(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "matrix.db")
	repo, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	applyMigrationsThrough(t, ctx, repo, "0029_cost_ledger.sql")
	populateDirtyUpgradeCorpus(t, ctx, repo, true)
	snapshotPath, err := repo.preMigrationSnapshot(ctx)
	if err != nil {
		repo.Close()
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(snapshotPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, []byte("corrupted live database"), 0o600); err != nil {
		t.Fatal(err)
	}
	copyFile(t, snapshotPath, dbPath)
	if err := os.Remove(dbPath + "-wal"); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Remove(dbPath + "-shm"); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	restored, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.IntegrityCheck(ctx); err != nil {
		t.Fatal(err)
	}
	if err := restored.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	assertUpgradeMatrix(t, ctx, restored)
}
