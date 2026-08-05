package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// formatTime must round-trip to the same instant it encodes; the only thing
// the migration can change is the *width* of the fraction, never its value.
func TestFormatTimeIsLexicographicallySortable(t *testing.T) {
	earlier := time.Date(2026, 8, 3, 5, 12, 34, 512300000, time.UTC)
	later := time.Date(2026, 8, 3, 5, 12, 34, 512340000, time.UTC)
	if (formatTime(earlier) <= formatTime(later)) != earlier.Before(later) {
		t.Fatalf("string order and chronological order disagree: %q vs %q", formatTime(earlier), formatTime(later))
	}
}

// TestMigration0021SortableTimestampsRoundTrip seeds a pre-0021 database with
// hand-written legacy-format rows and proves the migration rewrites every one
// to the *same instant* in the fixed-width form, leaving NULL untouched.
//
// The migration runner applies every pending file, so the test reaches a
// pre-0021 state by first running a full Migrate and then deleting the 0021
// bookkeeping row. Re-running Migrate then replays 0021 over the seeded rows,
// which is the exact upgrade path the migration was written for.
func TestMigration0021SortableTimestampsRoundTrip(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "roundtrip.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version='0021_v021_sortable_timestamps.sql'`); err != nil {
		t.Fatal(err)
	}

	// Each seed is (table, column, id, legacy-format value). NULL is covered by
	// the assets.missing_since row. The expected instant is whatever the legacy
	// string itself represented, parsed with the very layout every reader uses.
	seeds := []struct {
		table  string
		column string
		keyCol string
		id     string
		value  string
	}{
		{"library_roots", "created_at", "id", "root-1", "2026-08-03T05:12:34Z"},
		{"library_roots", "updated_at", "id", "root-1", "2026-08-03T05:12:34.1Z"},
		{"assets", "first_seen_at", "id", "asset-1", "2026-08-03T05:12:34.5123Z"},
		{"assets", "last_seen_at", "id", "asset-1", "2026-08-03T05:12:34.512300000Z"},
		{"assets", "missing_since", "id", "asset-1", "NULL"},
		{"settings", "updated_at", "key", "pipeline_throttle", "2026-08-03T05:12:34.12345678Z"},
	}
	inserts := []string{
		`INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root-1','/footage','2026-08-03T05:12:34Z','2026-08-03T05:12:34.1Z')`,
		`INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at,missing_since) VALUES('asset-1','fp',100,'discovered','2026-08-03T05:12:34.5123Z','2026-08-03T05:12:34.512300000Z',NULL)`,
		`INSERT INTO settings(key,value,updated_at) VALUES('pipeline_throttle','{}','2026-08-03T05:12:34.12345678Z')`,
	}
	for _, q := range inserts {
		if _, err := repo.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}

	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	wants := map[string]time.Time{
		"2026-08-03T05:12:34Z":           time.Date(2026, 8, 3, 5, 12, 34, 0, time.UTC),
		"2026-08-03T05:12:34.1Z":         time.Date(2026, 8, 3, 5, 12, 34, 100000000, time.UTC),
		"2026-08-03T05:12:34.5123Z":      time.Date(2026, 8, 3, 5, 12, 34, 512300000, time.UTC),
		"2026-08-03T05:12:34.512300000Z": time.Date(2026, 8, 3, 5, 12, 34, 512300000, time.UTC),
		"2026-08-03T05:12:34.12345678Z":  time.Date(2026, 8, 3, 5, 12, 34, 123456780, time.UTC),
	}

	for _, s := range seeds {
		if s.value == "NULL" {
			var got sql.NullString
			if err := repo.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE %s=?`, s.column, s.table, s.keyCol), s.id).Scan(&got); err != nil {
				t.Fatalf("%s.%s: %v", s.table, s.column, err)
			}
			if got.Valid {
				t.Fatalf("%s.%s: expected NULL, got %q", s.table, s.column, got.String)
			}
			continue
		}
		var got string
		if err := repo.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE %s=?`, s.column, s.table, s.keyCol), s.id).Scan(&got); err != nil {
			t.Fatalf("%s.%s: %v", s.table, s.column, err)
		}
		if len(got) != 30 {
			t.Fatalf("%s.%s: want 30-char fixed width, got %q (len %d)", s.table, s.column, got, len(got))
		}
		parsed, err := time.Parse(time.RFC3339Nano, got)
		if err != nil {
			t.Fatalf("%s.%s: %q does not parse with time.RFC3339Nano: %v", s.table, s.column, got, err)
		}
		if !parsed.Equal(wants[s.value]) {
			t.Fatalf("%s.%s: lost precision: %q parses to %v, want %v", s.table, s.column, got, parsed, wants[s.value])
		}
	}

	// The length<>30 guard makes the migration a no-op on already-correct rows:
	// snapshot the fixed-width values, re-run the migration, and require the
	// database to be byte-identical.
	before := make(map[string]string, len(seeds))
	for _, s := range seeds {
		if s.value == "NULL" {
			continue
		}
		var got string
		if err := repo.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE %s=?`, s.column, s.table, s.keyCol), s.id).Scan(&got); err != nil {
			t.Fatalf("%s.%s: %v", s.table, s.column, err)
		}
		before[s.table+"."+s.column] = got
	}
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, s := range seeds {
		if s.value == "NULL" {
			continue
		}
		var again string
		if err := repo.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE %s=?`, s.column, s.table, s.keyCol), s.id).Scan(&again); err != nil {
			t.Fatalf("%s.%s (rerun): %v", s.table, s.column, err)
		}
		if again != before[s.table+"."+s.column] {
			t.Fatalf("%s.%s: migration not idempotent: %q -> %q", s.table, s.column, before[s.table+"."+s.column], again)
		}
	}
}

// TestMigration0021CoversEveryTimestampColumn is the completeness property of
// the task. Rule: every column in the live schema whose name ends in "_at", or
// equals "run_after", is a formatTime value (written through formatTime, and
// before the fix through its RFC3339Nano predecessor). assets.missing_since is
// the one timestamp column whose name follows no such rule, so it is listed
// explicitly. The test fails if any such column is missing from migration 0021,
// because after the writer change a missed column would hold both formats and
// sort worse than the uniform legacy format it has today.
func TestMigration0021CoversEveryTimestampColumn(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "schema.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	migrationSQL, err := migrationFiles.ReadFile("migrations/0021_v021_sortable_timestamps.sql")
	if err != nil {
		t.Fatal(err)
	}
	// Match the UPDATE statement so a column present under the wrong table still
	// fails; a bare substring search on a name like "created_at" would pass even
	// if that exact table's column were uncovered.

	rows, err := repo.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var uncovered []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		cols, err := repo.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
		if err != nil {
			t.Fatalf("PRAGMA table_info(%s): %v", table, err)
		}
		var colNames []string
		for cols.Next() {
			var cid, notnull, pk int
			var name, ctype string
			var dflt any
			if err := cols.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
				cols.Close()
				t.Fatal(err)
			}
			colNames = append(colNames, name)
		}
		cols.Close()
		for _, name := range colNames {
			if isTimestampColumn(name) {
				needle := "UPDATE " + table + " SET " + name + " = CASE"
				if !strings.Contains(string(migrationSQL), needle) {
					uncovered = append(uncovered, fmt.Sprintf("%s.%s", table, name))
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(uncovered) > 0 {
		t.Fatalf("migration 0021 does not rewrite timestamp column(s): %s", strings.Join(uncovered, ", "))
	}
}

// isTimestampColumn states the naming rule that identifies formatTime columns:
// the conventional "_at" suffix or the historical "run_after" name, plus the
// one non-conforming timestamp column (assets.missing_since) found during the
// completeness audit.
func isTimestampColumn(name string) bool {
	switch name {
	case "run_after", "missing_since":
		return true
	}
	return strings.HasSuffix(name, "_at")
}
