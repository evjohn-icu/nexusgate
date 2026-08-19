package sqlite

import (
	"context"
	"strings"
	"testing"
)

func TestAssetShotOrdinalIndexMigrationAndNeighborPlans(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(t.TempDir() + "/ordinal-index.db")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	var indexSQL string
	if err := repo.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_asset_shots_asset_ordinal'`).Scan(&indexSQL); err != nil {
		t.Fatalf("ordinal index was not applied: %v", err)
	}
	if !strings.Contains(indexSQL, "asset_id, ordinal") {
		t.Fatalf("unexpected ordinal index definition: %q", indexSQL)
	}

	for _, tc := range []struct {
		name  string
		query string
	}{
		{
			name:  "previous",
			query: `SELECT id FROM asset_shots WHERE asset_id=? AND ordinal < ? ORDER BY ordinal DESC LIMIT 1`,
		},
		{
			name:  "next",
			query: `SELECT id FROM asset_shots WHERE asset_id=? AND ordinal > ? ORDER BY ordinal ASC LIMIT 1`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := repo.db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+tc.query, "asset", 10)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var details []string
			for rows.Next() {
				var id, parent, notused int
				var detail string
				if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
					t.Fatal(err)
				}
				details = append(details, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(details, " | ")
			if !strings.Contains(joined, "idx_asset_shots_asset_ordinal") {
				t.Fatalf("query plan does not use ordinal index: %s", joined)
			}
			if strings.Contains(strings.ToUpper(joined), "TEMP B-TREE") {
				t.Fatalf("query plan requires temporary sort: %s", joined)
			}
		})
	}
}
