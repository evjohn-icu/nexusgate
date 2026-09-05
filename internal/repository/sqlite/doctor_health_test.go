package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/search"
)

// openDoctorHealthRepo is the standard real-sqlite fixture: Open + Migrate,
// so the fts_index_state row exists and ensureCJKBigramFTS has marked it
// ready — the state every real Hub starts a doctor run in.
func openDoctorHealthRepo(t *testing.T) *Repository {
	t.Helper()
	repo, err := Open(filepath.Join(t.TempDir(), "doctor-health.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return repo
}

func seedShotForDoctorHealth(t *testing.T, repo *Repository, assetID, shotID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,100,'discovered','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`, assetID, "fp-"+assetID); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceAssetShots(ctx, assetID, "", []domain.AssetShot{
		{ID: shotID, AssetID: assetID, Ordinal: 0, StartMS: 0, EndMS: 1000, Description: "seeded shot"},
	}, "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestSearchIndexHealthFreshAndSeeded(t *testing.T) {
	repo := openDoctorHealthRepo(t)
	ctx := context.Background()

	health, err := repo.SearchIndexHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health.ShotCount != 0 || health.EmbeddingsCount != 0 || health.EmbeddingsShotCount != 0 {
		t.Fatalf("fresh index = %+v, want all zeros", health)
	}
	if health.FTSState != "ready" {
		t.Fatalf("fresh FTS state = %q, want ready (Migrate runs the rebuild)", health.FTSState)
	}

	seedShotForDoctorHealth(t, repo, "asset-dr-1", "shot-dr-1")
	seedShotForDoctorHealth(t, repo, "asset-dr-2", "shot-dr-2")
	// Embed only shot-dr-1: EmbeddingsCount counts vector rows,
	// EmbeddingsShotCount counts distinct shots carrying one.
	if err := repo.UpsertShotTextEmbeddings(ctx, []search.ShotEmbeddingRow{
		{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "shot-dr-1"}}, Model: "test-model", Vector: []float32{0.5, 0.25}, SourceTextHash: "h1"},
	}); err != nil {
		t.Fatal(err)
	}

	health, err = repo.SearchIndexHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health.ShotCount != 2 {
		t.Fatalf("shot count = %d, want 2", health.ShotCount)
	}
	if health.EmbeddingsCount != 1 || health.EmbeddingsShotCount != 1 {
		t.Fatalf("embeddings = %+v, want 1 row / 1 distinct shot", health)
	}
	if health.FTSState != "ready" {
		t.Fatalf("FTS state = %q, want ready", health.FTSState)
	}
}

// TestSearchIndexHealthFTSStateMapsFlag documents the three states doctor
// renders: 'ready' (healthy), 'pending' (rebuild owed — ensureCJKBigramFTS
// flips it back on next Migrate), and 'missing' (no index ever needed).
func TestSearchIndexHealthFTSStateMapsFlag(t *testing.T) {
	repo := openDoctorHealthRepo(t)
	ctx := context.Background()

	if _, err := repo.db.ExecContext(ctx, `UPDATE fts_index_state SET value='pending' WHERE name='cjk_bigram_v1'`); err != nil {
		t.Fatal(err)
	}
	health, err := repo.SearchIndexHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health.FTSState != "pending" {
		t.Fatalf("FTS state = %q, want pending", health.FTSState)
	}

	if _, err := repo.db.ExecContext(ctx, `DELETE FROM fts_index_state WHERE name='cjk_bigram_v1'`); err != nil {
		t.Fatal(err)
	}
	health, err = repo.SearchIndexHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health.FTSState != "missing" {
		t.Fatalf("FTS state = %q, want missing when the row is absent", health.FTSState)
	}
}

// TestMigrationStatusFresh asserts the ledger view doctor renders: every
// embedded migration applied, applied==total, and the newest filename is the
// schema version.
func TestMigrationStatusFresh(t *testing.T) {
	repo := openDoctorHealthRepo(t)
	status, err := repo.MigrationStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Total == 0 {
		t.Fatal("embedded migration count must be non-zero")
	}
	if status.Applied != status.Total {
		t.Fatalf("applied=%d total=%d: a fresh migrate must apply everything", status.Applied, status.Total)
	}
	if status.LastApplied == "" {
		t.Fatal("fresh migrate must record the last migration filename")
	}
}

// TestMigrationStatusReportsMissingRows pins the stale-library view: a row
// deleted from schema_migrations shows up as fewer applied migrations, not a
// lie that everything is up to date.
func TestMigrationStatusReportsMissingRows(t *testing.T) {
	repo := openDoctorHealthRepo(t)
	ctx := context.Background()

	var newest string
	if err := repo.db.QueryRowContext(ctx, `SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&newest); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version=?`, newest); err != nil {
		t.Fatal(err)
	}

	status, err := repo.MigrationStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Applied != status.Total-1 {
		t.Fatalf("applied=%d total=%d, want one missing row", status.Applied, status.Total)
	}
	if status.LastApplied == newest {
		t.Fatalf("last applied = %q, want anything but the deleted %q", status.LastApplied, newest)
	}
}
