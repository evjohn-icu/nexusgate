package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

func openSearchFacetTestRepo(t *testing.T, name string) *Repository {
	t.Helper()
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return repo
}

// seedSearchFacetFixtures builds n assets that all share one FTS-searchable
// summary token (so an unfiltered Search matches every one of them) and no
// asset_type yet — callers assign asset_type afterward to control which
// subset a facet narrows to. Every fixture also gets a scene tag unique to
// itself, so tests that need to isolate the tag-UNION path from the FTS
// fallback can search on that instead of the shared token.
func seedSearchFacetFixtures(t *testing.T, repo *Repository, n int, token string) []string {
	t.Helper()
	ctx := context.Background()
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT OR IGNORE INTO library_roots(id,path,created_at,updated_at) VALUES('search-facet-root','/footage',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("search-facet-asset-%02d", i)
		ids[i] = id
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'discovered',?,?)`, id, id, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,'search-facet-root',?,?,1,1,1,?)`,
			"loc-"+id, id, id+".mp4", "/footage/"+id+".mp4", now); err != nil {
			t.Fatal(err)
		}
		runID, _, err := repo.CreateModelRun(ctx, id, "vision", "fixture", "fixture-model", "hash-"+id, "facet-prompt-v1", "asset-analysis/v1", "{}")
		if err != nil {
			t.Fatal(err)
		}
		analysis := domain.StructuredAnalysis{Summary: token + " common phrase " + id}
		if err := repo.StageModelRun(ctx, runID, "{}", "{}"); err != nil {
			t.Fatal(err)
		}
		if err := repo.CommitAnalysis(ctx, id, runID, "asset-analysis/v1", analysis); err != nil {
			t.Fatal(err)
		}
		if err := repo.RebuildSearch(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

func setSearchFacetAssetType(t *testing.T, repo *Repository, assetID, assetType string) {
	t.Helper()
	if _, err := repo.db.ExecContext(context.Background(), `UPDATE asset_analysis SET asset_type=? WHERE asset_id=?`, assetType, assetID); err != nil {
		t.Fatal(err)
	}
}

// TestSearchFilteredLimitCountsFacetMatchingRowsNotFirstHits is the point of
// O1: LIMIT must count facet-matching rows, not be applied to the first
// `limit` unfiltered hits and then filtered. Six assets all match the query;
// the fixture assigns the facet to whichever three sort *after* the limit
// boundary in Search's own unfiltered order, discovered by actually calling
// Search first rather than assumed — so this does not depend on guessing how
// SQLite orders an un-ORDER-BY'd result set. A post-filter implementation
// fetches only the first 3 (positions with no facet) and returns nothing;
// the in-query EXISTS guard returns all 3 true facet matches regardless of
// where they sorted.
func TestSearchFilteredLimitCountsFacetMatchingRowsNotFirstHits(t *testing.T) {
	repo := openSearchFacetTestRepo(t, "search-facets-limit.db")
	ctx := context.Background()
	seedSearchFacetFixtures(t, repo, 6, "limittoken")

	natural, err := repo.Search(ctx, "limittoken", 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(natural) != 6 {
		t.Fatalf("natural order=%v, want all 6 seeded ids matched unfiltered", natural)
	}

	const limit = 3
	facetIDs := map[string]bool{}
	for _, id := range natural[limit:] {
		facetIDs[id] = true
		setSearchFacetAssetType(t, repo, id, "b_roll")
	}

	got, err := repo.SearchFiltered(ctx, "limittoken", limit, domain.FacetFilter{AssetTypes: []string{"b_roll"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != limit {
		t.Fatalf("SearchFiltered returned %d ids, want %d (the exhaustive facet-matching set): %v", len(got), limit, got)
	}
	for _, id := range got {
		if !facetIDs[id] {
			t.Fatalf("SearchFiltered returned %s, which was never assigned the b_roll facet: %v", id, got)
		}
	}
}

// TestSearchFilteredZeroFacetProducesTodaysSQL proves — by capturing the
// literal SQL text SearchFiltered sends to SQLite via searchSQLTrace and
// diffing it against the query text Search used before facets existed — that
// a zero domain.FacetFilter changes no query plan. Asserting on results
// alone would not catch e.g. an "AND 1=1" guard that returns the same rows
// through a different plan; the query plans are exactly what the comment
// above SearchFiltered's tag-UNION construction was written to protect.
func TestSearchFilteredZeroFacetProducesTodaysSQL(t *testing.T) {
	repo := openSearchFacetTestRepo(t, "search-facets-zero-sql.db")
	ctx := context.Background()
	seedSearchFacetFixtures(t, repo, 1, "zerofacettoken")

	const wantTagQuery = `SELECT asset_id FROM (
SELECT l.asset_id FROM asset_tag_links l WHERE l.normalized_tag=?
UNION
SELECT l.asset_id FROM asset_tag_links l JOIN tag_catalog t ON t.id=l.canonical_tag_id WHERE t.canonical_name=?
UNION
SELECT l.asset_id FROM asset_tag_links l JOIN tag_aliases_v2 a ON a.canonical_tag_id=l.canonical_tag_id WHERE a.alias_normalized=?
)
LIMIT ?`
	const wantFTSQuery = `SELECT asset_id FROM asset_search WHERE asset_search MATCH ? ORDER BY bm25(asset_search) LIMIT ?`

	var captured []string
	searchSQLTrace = func(q string) { captured = append(captured, q) }
	t.Cleanup(func() { searchSQLTrace = nil })

	if _, err := repo.SearchFiltered(ctx, "zerofacettoken", 10, domain.FacetFilter{}); err != nil {
		t.Fatal(err)
	}

	if len(captured) < 2 {
		t.Fatalf("SearchFiltered issued %d queries, want at least the tag-union query and the FTS fallback: %v", len(captured), captured)
	}
	last := captured[len(captured)-1]
	if last != wantFTSQuery {
		t.Fatalf("FTS fallback SQL changed for a zero FacetFilter:\ngot:  %q\nwant: %q", last, wantFTSQuery)
	}
	for _, q := range captured[:len(captured)-1] {
		if q != wantTagQuery {
			t.Fatalf("tag UNION SQL changed for a zero FacetFilter:\ngot:  %q\nwant: %q", q, wantTagQuery)
		}
	}
}

// TestSearchFilteredZeroFacetMatchesSearch is the behavioral half of the
// same guarantee: over a fixture with both a tag hit and an FTS hit, Search
// and SearchFiltered(zero FacetFilter) must return identical results. Search
// delegates to SearchFiltered now, so this also guards against a future edit
// breaking that delegation without either function's own tests noticing.
func TestSearchFilteredZeroFacetMatchesSearch(t *testing.T) {
	repo := openSearchFacetTestRepo(t, "search-facets-zero-behavior.db")
	ctx := context.Background()
	now := formatTime(time.Now().UTC())

	// A tag hit: asset-tag-hit is only found through asset_tag_links, not FTS.
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-tag-hit','fp',1,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	analysis := domain.StructuredAnalysis{SceneTags: []string{"稀有场景标签"}}
	if err := repo.SyncAnalysisTags(ctx, "asset-tag-hit", "run-tag", analysis); err != nil {
		t.Fatal(err)
	}

	// An FTS hit: asset-fts-hit is only found through asset_search.
	ids := seedSearchFacetFixtures(t, repo, 1, "ftsonlytoken")
	ftsID := ids[0]

	for _, q := range []string{"稀有场景标签", "ftsonlytoken"} {
		want, err := repo.Search(ctx, q, 10)
		if err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		got, err := repo.SearchFiltered(ctx, q, 10, domain.FacetFilter{})
		if err != nil {
			t.Fatalf("SearchFiltered(%q): %v", q, err)
		}
		if len(want) != len(got) {
			t.Fatalf("query %q: Search=%v SearchFiltered(zero)=%v", q, want, got)
		}
		for i := range want {
			if want[i] != got[i] {
				t.Fatalf("query %q: Search=%v SearchFiltered(zero)=%v", q, want, got)
			}
		}
	}
	if len(ids) != 1 || ftsID == "" {
		t.Fatalf("fixture setup: ids=%v", ids)
	}
}

// TestSearchFilteredFTSOrderedByRelevance proves the FTS fallback orders hits
// by bm25 relevance (most relevant first) rather than FTS5's internal docid.
// Two assets share the same keyword but with different term frequency: the
// higher-frequency one must sort first. This is the asset-level counterpart
// to the shot-level `ORDER BY bm25(asset_shot_search)`.
func TestSearchFilteredFTSOrderedByRelevance(t *testing.T) {
	repo := openSearchFacetTestRepo(t, "search-facets-relevance.db")
	ctx := context.Background()
	now := formatTime(time.Now().UTC())

	if _, err := repo.db.ExecContext(ctx, `INSERT OR IGNORE INTO library_roots(id,path,created_at,updated_at) VALUES('relevance-root','/footage',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	seedRelevanceAsset := func(id, summary string) {
		t.Helper()
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'discovered',?,?)`, id, id, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,'relevance-root',?,?,1,1,1,?)`,
			"loc-"+id, id, id+".mp4", "/footage/"+id+".mp4", now); err != nil {
			t.Fatal(err)
		}
		runID, _, err := repo.CreateModelRun(ctx, id, "vision", "fixture", "fixture-model", "hash-"+id, "facet-prompt-v1", "asset-analysis/v1", "{}")
		if err != nil {
			t.Fatal(err)
		}
		analysis := domain.StructuredAnalysis{Summary: summary}
		if err := repo.StageModelRun(ctx, runID, "{}", "{}"); err != nil {
			t.Fatal(err)
		}
		if err := repo.CommitAnalysis(ctx, id, runID, "asset-analysis/v1", analysis); err != nil {
			t.Fatal(err)
		}
		if err := repo.RebuildSearch(ctx, id); err != nil {
			t.Fatal(err)
		}
	}

	// Both assets match the keyword "sunset", but asset-relevance-high mentions
	// it far more often, so it should rank above asset-relevance-low under bm25.
	seedRelevanceAsset("asset-relevance-high", strings.Repeat("sunset ", 12)+"golden hour wide shot")
	seedRelevanceAsset("asset-relevance-low", "one sunset at the beach")

	got, err := repo.Search(ctx, "sunset", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Search(sunset) returned %d ids, want 2: %v", len(got), got)
	}
	if got[0] != "asset-relevance-high" {
		t.Fatalf("Search(sunset) relevance order = %v, want asset-relevance-high first (bm25 should rank higher term frequency above lower)", got)
	}
}
