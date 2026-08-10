package sqlite

import (
	"context"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestMigration0033BackfillsEarliestLiveLocation(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "migration-0033.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	old := "2000-01-01T00:00:00.000000000Z"
	newer := "2000-01-02T00:00:00.000000000Z"
	if _, err := repo.db.ExecContext(ctx, `CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if strings.Compare(entry.Name(), "0033_asset_probe_identity.sql") >= 0 {
			break
		}
		content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.ExecContext(ctx, string(content)); err != nil {
			t.Fatalf("apply %s: %v", entry.Name(), err)
		}
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`, entry.Name(), newer); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.db.Exec(`INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-m33','fp-m33',1,'discovered',?,?)`, old, newer); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(`INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root-m33','/tmp/m33',?,?)`, old, newer); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(`INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES('loc-old','asset-m33','root-m33','old.mp4','/tmp/old',111,0,1,?)`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(`INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES('loc-live','asset-m33','root-m33','live.mp4','/tmp/live',222,1,1,?)`, newer); err != nil {
		t.Fatal(err)
	}
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := repo.db.QueryRow(`SELECT probe_modified_ns FROM assets WHERE id='asset-m33'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "222" {
		t.Fatalf("probe_modified_ns=%q, want live location mtime 222", got)
	}
}
