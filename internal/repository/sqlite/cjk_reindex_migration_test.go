package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"
)

// legacyBuggySegment reproduces internal/textindex.Chunks exactly as it was
// before 8fb1dd0 ("Stop a CJK run from being swallowed by an adjacent ASCII
// word token"): the ASCII word scan had no `&& !isCJK(...)` guard, so a word
// run absorbed any CJK immediately following it instead of yielding to the
// bigram branch. It exists only to seed a test row the way a pre-fix Hub
// actually wrote it -- a hand-typed literal would be asserting the bug from
// memory instead of from the diff.
func legacyBuggySegment(text string) string {
	isCJK := func(r rune) bool { return r >= '㐀' && r <= '鿿' } // matches internal/textindex.isCJK
	isWord := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }
	runes := []rune(strings.ToLower(strings.TrimSpace(text)))
	var tokens []string
	for i := 0; i < len(runes); {
		if isCJK(runes[i]) {
			start := i
			for i < len(runes) && isCJK(runes[i]) {
				i++
			}
			seq := runes[start:i]
			if len(seq) == 1 {
				tokens = append(tokens, string(seq))
			} else {
				for j := 0; j+1 < len(seq); j++ {
					tokens = append(tokens, string(seq[j:j+2]))
				}
			}
			continue
		}
		if isWord(runes[i]) {
			start := i
			for i < len(runes) && isWord(runes[i]) { // pre-fix: no CJK guard
				i++
			}
			tokens = append(tokens, string(runes[start:i]))
			continue
		}
		i++
	}
	return strings.Join(tokens, " ")
}

// openCJKReindexTestRepo mirrors the Open+Migrate pattern used throughout
// this package (see search_filtered_facets_test.go); a fake repository would
// not exercise fts_index_state or FTS5 at all, and those are the whole point
// here.
func openCJKReindexTestRepo(t *testing.T, name string) *Repository {
	t.Helper()
	repo, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return repo
}

// seedStaleCJKAsset inserts one asset whose asset_analysis.summary is the
// real (correctly encoded) Chinese text, but whose asset_search.summary is
// written with legacyBuggySegment instead of indexText -- simulating a row
// that was tokenized by the Hub before 8fb1dd0 shipped and has sat untouched
// in a library whose fts_index_state row already reached 'ready' (that flag
// has meant "the bigram shadow tables exist" since migrations/0012, not
// "every row in them was tokenized by the current segmenter"). It bypasses
// RebuildSearch/indexText on purpose: calling either would use today's fixed
// segmenter and defeat the point of the fixture.
func seedStaleCJKAsset(t *testing.T, repo *Repository, id, summary string) {
	t.Helper()
	ctx := context.Background()
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT OR IGNORE INTO library_roots(id,path,created_at,updated_at) VALUES('cjk-reindex-root','/footage',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'discovered',?,?)`, id, id, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,'cjk-reindex-root',?,?,1,1,1,?)`,
		"loc-"+id, id, id+".mp4", "/footage/"+id+".mp4", now); err != nil {
		t.Fatal(err)
	}
	runID := "run-" + id
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO model_runs(id,asset_id,capability,provider,model,input_hash,prompt_version,schema_version,state,request_json,started_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		runID, id, "vision", "fixture", "fixture-model", "hash-"+id, "cjk-reindex-prompt-v1", "asset-analysis/v1", "validated", "{}", now); err != nil {
		t.Fatal(err)
	}
	// The source of truth (asset_analysis.summary) already holds the real
	// text -- rebuildCJKBigramFTS re-derives asset_search from this table,
	// not from the stale asset_search row itself, so this is what makes the
	// green step below possible without touching asset_analysis again.
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_analysis(asset_id,source_run_id,schema_version,asset_type,shot_size,camera_motion,audio_type,lighting,people_count,has_speech,quality,summary,scene_tags_json,subjects_json,mood_tags_json,usable_as_json,quality_flags_json,extra_tags_json,editorial_reason,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, runID, "asset-analysis/v1", "clip", "wide", "static", "none", "daylight", 0, 0, "usable",
		summary, "[]", "[]", "[]", "[]", "[]", "[]", "", now); err != nil {
		t.Fatal(err)
	}
	// Legacy write path: real filename/location text through indexText (it
	// has no CJK in this fixture so the bug does not touch it), but the
	// summary through legacyBuggySegment -- exactly what asset_search held
	// under the pre-fix segmenter.
	res, err := repo.db.ExecContext(ctx, `INSERT INTO asset_search(asset_id,filename,summary,transcript,scene_tags,subjects,mood_tags,extra_tags,location,editorial_reason) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		id, indexText(id+".mp4"), legacyBuggySegment(summary), "", "", "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	rowid, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_search_rowids(asset_id,search_rowid) VALUES(?,?)`, id, rowid); err != nil {
		t.Fatal(err)
	}
}

// migrationFileName is the exact filename Migrate() tracks in
// schema_migrations for 0022 -- kept as one constant so the red/green pair
// below cannot typo two different strings past each other.
const cjkReindexMigrationFile = "0022_v021_cjk_bigram_reindex.sql"

// TestStaleCJKIndexRowIsUnsearchableUntilMigrationReruns is the red/green
// pair the migration exists to fix. Step 1 proves the bug still stands today
// for any row written before 8fb1dd0: fts_index_state reaching 'ready' (set
// once, back when migrations/0012 first ran) does not mean every row in
// asset_search was tokenized by the segmenter currently in the binary, and
// nothing revisits a row just because the binary changed underneath it.
// Step 2 proves 0022 is what recovers it: schema_migrations' tracking row
// for 0022 is deleted to put this database back in the state a real
// pre-existing library is in the first time it runs a binary that ships
// 0022 (the file has never been recorded as applied on it before), then
// Migrate() is called again the same way cmd/nexusgate/main.go calls it on
// every startup.
func TestStaleCJKIndexRowIsUnsearchableUntilMigrationReruns(t *testing.T) {
	ctx := context.Background()
	repo := openCJKReindexTestRepo(t, "cjk-reindex.db")

	const assetID = "cjk-reindex-asset-1"
	const summary = "2024年春节的素材"
	seedStaleCJKAsset(t, repo, assetID, summary)

	// Sanity check on the fixture itself: the buggy segmenter really did
	// produce one unsplit token for this input, so the red assertion below
	// is caused by the CJK bug and not by some other seeding mistake.
	if got := legacyBuggySegment(summary); got != strings.ToLower(summary) {
		t.Fatalf("fixture sanity: legacyBuggySegment(%q) = %q, want the whole string as one token", summary, got)
	}

	// Step 1 (red): today, a stale row sitting in a 'ready' index is
	// invisible to a query built with the current (fixed) FTSQuery.
	hits, err := repo.Search(ctx, "春节", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("fixture sanity failed: stale row already matched (got %v) before the migration ran -- the red case never held", hits)
	}

	// Put the database back in "0022 has never run here" state, then let
	// Migrate() apply it and drive ensureCJKBigramFTS the same way a real
	// Hub startup would.
	if _, err := repo.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = ?`, cjkReindexMigrationFile); err != nil {
		t.Fatal(err)
	}
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// Step 2 (green): the same query now finds the asset, because
	// rebuildCJKBigramFTS re-tokenized asset_analysis.summary with today's
	// segmenter and overwrote the stale asset_search row.
	hits, err = repo.Search(ctx, "春节", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range hits {
		if id == assetID {
			found = true
		}
	}
	if !found {
		t.Fatalf("Search(%q) = %v, want it to contain %q after the migration reran the rebuild", "春节", hits, assetID)
	}
}
