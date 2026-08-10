package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestTotalSourceBytesSumAcrossAssets pins the aggregation behind the storage
// overview's 原片（估算）row: the SUM of assets.file_size is the whole answer,
// and a library with no assets is 0, not an error or NULL.
func TestTotalSourceBytesSumAcrossAssets(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "storage-health.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	empty, err := repo.TotalSourceBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if empty != 0 {
		t.Fatalf("empty library total = %d, want 0", empty)
	}

	now := formatTime(time.Now().UTC())
	for _, asset := range []struct {
		id   string
		size int64
	}{{"asset-a", 100}, {"asset-b", 250}, {"asset-c", 1 << 20}} {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,?,'discovered',?,?)`, asset.id, "fp-"+asset.id, asset.size, now, now); err != nil {
			t.Fatal(err)
		}
	}

	total, err := repo.TotalSourceBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(100 + 250 + (1 << 20)); total != want {
		t.Fatalf("total = %d, want %d", total, want)
	}
}
