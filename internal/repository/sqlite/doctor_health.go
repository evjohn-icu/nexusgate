package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"sort"
	"strings"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

// SearchIndexHealth is the read-only search-index view `nexusslate doctor`
// renders. It counts what the retrieval layer actually has — canonical shot
// rows, text-embedding vectors and the distinct shots carrying one — and
// reads the fts_index_state flag ensureCJKBigramFTS maintains. The FTS flag
// answers "is the lexical index known-good": 'ready' after a successful
// rebuild, 'pending' when a rebuild is owed, and a missing row means the
// index was never needed. This method never rebuilds anything; it is a probe.
func (r *Repository) SearchIndexHealth(ctx context.Context) (domain.SearchIndexHealth, error) {
	var health domain.SearchIndexHealth
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_shots`).Scan(&health.ShotCount); err != nil {
		return health, err
	}
	// EmbeddingsShotCount uses DISTINCT because the row is a cache keyed by
	// shot_id; counting distinct shots is what the operator cares about even
	// if a future write ever duplicates rows.
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(DISTINCT shot_id) FROM shot_text_embeddings`).Scan(&health.EmbeddingsCount, &health.EmbeddingsShotCount); err != nil {
		return health, err
	}
	var state string
	err := r.db.QueryRowContext(ctx, `SELECT value FROM fts_index_state WHERE name='cjk_bigram_v1'`).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		health.FTSState = "missing"
		return health, nil
	}
	if err != nil {
		return health, err
	}
	health.FTSState = state
	return health, nil
}

// MigrationStatus is the schema_migrations view `nexusslate doctor` renders in
// the DB section: the embedded migration count this binary ships, how many
// the library has applied, and the newest applied filename — the schema
// version an operator can compare against a release note. It mirrors
// Migrate's own loop so the two cannot drift.
func (r *Repository) MigrationStatus(ctx context.Context) (domain.MigrationStatus, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return domain.MigrationStatus{}, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var status domain.MigrationStatus
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			status.Total++
		}
	}
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE((SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1), '') FROM schema_migrations`).Scan(&status.Applied, &status.LastApplied); err != nil {
		return domain.MigrationStatus{}, err
	}
	return status, nil
}
