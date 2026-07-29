package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ev/timingdex/internal/domain"
)

func TestTagCurationApprovalAndAliasSearchIntegration(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "timingdex-test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	now := formatTime(time.Now())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root-1','/footage',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-1','fp',100,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES('loc-1','asset-1','root-1','night.mp4','/footage/night.mp4',1,1,1,?)`, now); err != nil {
		t.Fatal(err)
	}

	analysis := domain.StructuredAnalysis{
		SceneTags: []string{"城市夜景", "Night City"},
	}
	if err := repo.SyncAnalysisTags(ctx, "asset-1", "run-1", analysis); err != nil {
		t.Fatal(err)
	}
	unresolved, err := repo.ListUnresolvedTags(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 2 {
		t.Fatalf("unresolved=%+v", unresolved)
	}

	result, err := repo.CreateTagCurationRun(ctx, []domain.TagProposal{{
		ProposalType:  "create_canonical_with_aliases",
		CanonicalName: "urban_night",
		Payload: map[string]any{
			"canonical_name": "urban_night",
			"aliases":        []string{"城市夜景", "night_city", "city_at_night"},
			"category":       "scene",
		},
		Confidence: 0.95, Reason: "integration fixture", AffectedAssets: 1,
	}}, len(unresolved), "fixture-small-lm")
	if err != nil {
		t.Fatal(err)
	}
	if result.Strategy != "fixture-small-lm" || result.ProposalsCreated != 1 {
		t.Fatalf("result=%+v", result)
	}
	proposals, err := repo.ListTagProposals(ctx, "pending", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 1 {
		t.Fatalf("proposals=%+v", proposals)
	}
	if err := repo.ReviewTagProposal(ctx, proposals[0].ID, "approve", "fixture approval"); err != nil {
		t.Fatal(err)
	}

	unresolved, err = repo.ListUnresolvedTags(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unresolved after approval=%+v", unresolved)
	}
	if err := repo.RebuildSearch(ctx, "asset-1"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"urban_night", "城市夜景", "city_at_night"} {
		ids, err := repo.Search(ctx, query, 10)
		if err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
		if len(ids) != 1 || ids[0] != "asset-1" {
			t.Fatalf("search %q=%v", query, ids)
		}
	}
}

func TestTagIntelligenceStorageIntegration(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "timingdex-test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	tags := []domain.UnresolvedTag{{NormalizedTag: "night_city", UsageCount: 3, AssetCount: 2}, {NormalizedTag: "urban_night", UsageCount: 2, AssetCount: 2}}
	if err := repo.UpsertTagEmbeddings(ctx, "fixture-embed", tags, [][]float64{{1, 0}, {0.99, 0.01}}); err != nil {
		t.Fatal(err)
	}
	runID, err := repo.CreateTagClusterRun(ctx, "fixture", "fixture-embed", .86, []domain.TagCluster{{Members: []string{"night_city", "urban_night"}, Similarity: .99}}, len(tags))
	if err != nil || runID == "" {
		t.Fatalf("run=%q err=%v", runID, err)
	}
	var embeddings, clusters int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tag_embeddings`).Scan(&embeddings); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tag_clusters WHERE run_id=?`, runID).Scan(&clusters); err != nil {
		t.Fatal(err)
	}
	if embeddings != 2 || clusters != 1 {
		t.Fatalf("embeddings=%d clusters=%d", embeddings, clusters)
	}

	input, err := repo.BuildLibrarySummaryInput(ctx)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := repo.SaveLibrarySummary(ctx, domain.LibrarySummary{Summary: "fixture summary", Themes: []string{"urban_night"}, SuitableFor: []string{"b-roll"}, Input: input, Provider: "fixture", Model: "fixture-model"})
	if err != nil {
		t.Fatal(err)
	}
	latest, err := repo.LatestLibrarySummary(ctx)
	if err != nil || latest == nil || latest.ID != saved.ID {
		t.Fatalf("latest=%+v err=%v", latest, err)
	}
}

func TestV015CaptureAndProviderMetadataMigrateWithoutSecrets(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "v015.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{
		"capture_metadata", "capture_evidence", "capture_sidecars",
		"shoot_sessions", "asset_shoot_sessions",
		"provider_channels", "provider_channel_members", "provider_channel_events",
	} {
		var name string
		err := repo.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil || name != table {
			t.Fatalf("migration did not create %s: name=%q err=%v", table, name, err)
		}
	}
	var persistedSecrets int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name LIKE '%secret%'`).Scan(&persistedSecrets); err != nil {
		t.Fatal(err)
	}
	if persistedSecrets != 0 {
		t.Fatalf("SQLite must not contain a provider secret table, found %d", persistedSecrets)
	}
}

func TestProviderChannelsPersistOnlySecretReferences(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "channels.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	written, err := repo.UpsertProviderChannel(ctx, domain.ProviderChannel{
		Capability: "video_analysis", Label: "Gemini Flash", ProviderName: "gemini",
		Protocol: "gemini_generate_content", Endpoint: "https://example.invalid/v1", Model: "gemini-flash", Enabled: true,
		Members: []domain.ProviderChannelMember{{Label: "key-a", SecretRef: "provider/gemini-a", Enabled: true, Weight: 1, MaxInflight: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if written.ID == "" || len(written.Members) != 1 || written.Members[0].SecretRef != "provider/gemini-a" {
		t.Fatalf("written=%+v", written)
	}
	channels, err := repo.ListProviderChannels(ctx, "video_analysis")
	if err != nil || len(channels) != 1 || channels[0].Members[0].SecretRef != "provider/gemini-a" {
		t.Fatalf("channels=%+v err=%v", channels, err)
	}
	var stored string
	if err := repo.db.QueryRowContext(ctx, `SELECT secret_ref FROM provider_channel_members WHERE id=?`, written.Members[0].ID).Scan(&stored); err != nil || stored != "provider/gemini-a" {
		t.Fatalf("secret reference=%q err=%v", stored, err)
	}
}

// TestSoftDeleteProviderChannelFreesLabelForReuse guards against a UNIQUE
// constraint regression: provider_channels has UNIQUE(capability,label), and
// deleted_at alone does not scope that constraint, so a naive soft-delete
// would permanently block recreating a channel with the same name.
func TestSoftDeleteProviderChannelFreesLabelForReuse(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "channel-relabel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	original, err := repo.UpsertProviderChannel(ctx, domain.ProviderChannel{
		Capability: "video_analysis", Label: "Reused Label", ProviderName: "gemini",
		Protocol: "gemini_generate_content", Endpoint: "https://example.invalid/v1", Model: "gemini-flash", Enabled: true,
		Members: []domain.ProviderChannelMember{{Label: "key-a", SecretRef: "provider/reuse-a", Enabled: true, Weight: 1, MaxInflight: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SoftDeleteProviderChannel(ctx, original.ID); err != nil {
		t.Fatal(err)
	}

	recreated, err := repo.UpsertProviderChannel(ctx, domain.ProviderChannel{
		Capability: "video_analysis", Label: "Reused Label", ProviderName: "gemini",
		Protocol: "gemini_generate_content", Endpoint: "https://example.invalid/v1", Model: "gemini-flash", Enabled: true,
		Members: []domain.ProviderChannelMember{{Label: "key-a", SecretRef: "provider/reuse-b", Enabled: true, Weight: 1, MaxInflight: 1}},
	})
	if err != nil {
		t.Fatalf("recreate with same capability+label should succeed after soft-delete: %v", err)
	}
	if recreated.ID == original.ID {
		t.Fatalf("recreated channel should be a distinct row, got same id %q", recreated.ID)
	}

	channels, err := repo.ListProviderChannels(ctx, "video_analysis")
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 1 {
		t.Fatalf("channels=%+v; want exactly the recreated channel", channels)
	}
	if channels[0].ID != recreated.ID || channels[0].Label != "Reused Label" {
		t.Fatalf("listed channel=%+v; want recreated channel with untouched label", channels[0])
	}
}

func TestSaveMediaMetadataProjectsCaptureFields(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "capture-projection.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-capture','fp',1,'discovered',?,?)`, formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveMediaMetadata(ctx, "asset-capture", domain.MediaMetadata{CapturedAt: &now, CaptureVendor: "DJI", CameraMake: "DJI", CameraModel: "Mavic 3", CameraSerial: "DJI-1", SourceColor: "LOG_UNKNOWN", ColorProfile: "D-Log", PreviewStatus: "lut_required"}, "capture-fixture"); err != nil {
		t.Fatal(err)
	}
	var vendor, model, profile, preview string
	if err := repo.db.QueryRowContext(ctx, `SELECT vendor,model,color_profile,preview_status FROM capture_metadata WHERE asset_id='asset-capture'`).Scan(&vendor, &model, &profile, &preview); err != nil {
		t.Fatal(err)
	}
	if vendor != "DJI" || model != "Mavic 3" || profile != "D-Log" || preview != "lut_required" {
		t.Fatalf("capture row=%q/%q/%q/%q", vendor, model, profile, preview)
	}
}

func TestRebuildAutomaticShootSessionsGroupsSameCameraWithinThirtyMinutes(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	stamp := formatTime(now)
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root-session','/footage',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"session-a", "session-b", "session-c"} {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,? ,1,'discovered',?,?)`, id, id, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?, 'root-session', ?, ?, 1,1,1,?)`, "loc-"+id, id, id+".mov", "/footage/"+id+".mov", stamp); err != nil {
			t.Fatal(err)
		}
	}
	for id, capturedAt := range map[string]time.Time{"session-a": now, "session-b": now.Add(20 * time.Minute), "session-c": now.Add(55 * time.Minute)} {
		if err := repo.SaveMediaMetadata(ctx, id, domain.MediaMetadata{CapturedAt: &capturedAt, CameraMake: "Sony", CameraModel: "FX3", CameraSerial: "serial-1", DurationMS: 5000}, "session-fixture"); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.RebuildAutomaticShootSessions(ctx, "root-session"); err != nil {
		t.Fatal(err)
	}
	var sessions, mappings int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM shoot_sessions WHERE root_id='root-session' AND state='automatic'`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_shoot_sessions`).Scan(&mappings); err != nil {
		t.Fatal(err)
	}
	if sessions != 2 || mappings != 3 {
		t.Fatalf("sessions=%d mappings=%d, want 2/3", sessions, mappings)
	}
}

// explainQueryPlan runs EXPLAIN QUERY PLAN and returns each step's detail
// text, in order -- the fourth column of the `id|parent|notused|detail` shape
// SQLite returns.
func explainQueryPlan(t *testing.T, repo *Repository, query string, args ...any) []string {
	t.Helper()
	rows, err := repo.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
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
	return details
}

// TestFTSDeletesResolveRowidInsteadOfScanningShadowTable proves the fix for
// the O(n^2) analyze-commit cost: asset_search and asset_shot_search declare
// asset_id UNINDEXED, so FTS5 builds no secondary index on it and a delete
// filtered by asset_id alone scans the whole shadow table (the "before"
// plans below, `SCAN ... VIRTUAL TABLE INDEX 0:` with no operator after the
// colon -- no usable constraint). deleteFromSearchIndexTx and
// deleteFromShotSearchIndexTx instead resolve the FTS5 rowid(s) through the
// new asset_search_rowids/asset_shot_search_rowids mapping tables first, so
// the actual FTS5 delete carries a rowid equality/IN constraint (`INDEX 0:=`)
// driven by an indexed lookup on the mapping table.
func TestFTSDeletesResolveRowidInsteadOfScanningShadowTable(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "fts-delete-plan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	beforeAssetSearch := explainQueryPlan(t, repo, `DELETE FROM asset_search WHERE asset_id=?`, "asset-x")
	if len(beforeAssetSearch) != 1 || beforeAssetSearch[0] != "SCAN asset_search VIRTUAL TABLE INDEX 0:" {
		t.Fatalf("expected the old asset_id delete to fully scan the shadow table, got %v", beforeAssetSearch)
	}

	afterAssetSearch := explainQueryPlan(t, repo, `DELETE FROM asset_search WHERE rowid IN (SELECT search_rowid FROM asset_search_rowids WHERE asset_id=?)`, "asset-x")
	assertRowidDrivenPlan(t, afterAssetSearch, "asset_search", "asset_search_rowids")

	beforeShotSearch := explainQueryPlan(t, repo, `DELETE FROM asset_shot_search WHERE asset_id=?`, "asset-x")
	if len(beforeShotSearch) != 1 || beforeShotSearch[0] != "SCAN asset_shot_search VIRTUAL TABLE INDEX 0:" {
		t.Fatalf("expected the old asset_id delete to fully scan the shot shadow table, got %v", beforeShotSearch)
	}

	afterShotSearch := explainQueryPlan(t, repo, `DELETE FROM asset_shot_search WHERE rowid IN (SELECT search_rowid FROM asset_shot_search_rowids WHERE asset_id=?)`, "asset-x")
	assertRowidDrivenPlan(t, afterShotSearch, "asset_shot_search", "asset_shot_search_rowids")
}

func assertRowidDrivenPlan(t *testing.T, plan []string, ftsTable, mappingTable string) {
	t.Helper()
	sawRowidSeek := false
	sawIndexedMappingLookup := false
	for _, detail := range plan {
		if strings.Contains(detail, ftsTable+" VIRTUAL TABLE INDEX 0:=") {
			sawRowidSeek = true
		}
		if strings.HasPrefix(detail, "SEARCH "+mappingTable+" USING INDEX") {
			sawIndexedMappingLookup = true
		}
		if detail == "SCAN "+ftsTable+" VIRTUAL TABLE INDEX 0:" {
			t.Fatalf("delete for %s still fully scans the shadow table: %v", ftsTable, plan)
		}
	}
	if !sawRowidSeek || !sawIndexedMappingLookup {
		t.Fatalf("expected a rowid-driven delete for %s via an indexed lookup on %s, got %v", ftsTable, mappingTable, plan)
	}
}

// TestRebuildSearchKeepsRowidMappingConsistentAcrossRebuilds exercises
// RebuildSearch and ReplaceAssetShots repeatedly (as pipeline reprocessing
// would) and checks the invariant the rowid mapping tables depend on: every
// FTS shadow-table row has exactly one row id (or shot_id) mapping, and vice
// versa. If a delete or insert path ever forgets to keep the mapping in
// sync, this invariant breaks either by leaving orphaned FTS rows behind (a
// slow leak that eventually reintroduces the O(n) scan) or by leaving stale
// mapping rows pointing at nothing.
func TestRebuildSearchKeepsRowidMappingConsistentAcrossRebuilds(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "fts-rowid-consistency.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root-rowid','/footage',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	assetIDs := []string{"rowid-asset-1", "rowid-asset-2", "rowid-asset-3"}
	for _, id := range assetIDs {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'discovered',?,?)`, id, id+"-fp", now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,'root-rowid',?,?,1,1,1,?)`, "loc-"+id, id, id+".mov", "/footage/"+id+".mov", now); err != nil {
			t.Fatal(err)
		}
		if err := repo.RebuildSearch(ctx, id); err != nil {
			t.Fatal(err)
		}
		if err := repo.ReplaceAssetShots(ctx, id, "", []domain.AssetShot{
			{StartMS: 0, EndMS: 1000, Description: "first shot for " + id},
			{StartMS: 1000, EndMS: 2000, Description: "second shot for " + id},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Reprocess one asset repeatedly, as re-analysis or a retried job would.
	// This is exactly the call pattern that used to full-scan the shadow
	// tables on every iteration.
	for i := 0; i < 5; i++ {
		if err := repo.RebuildSearch(ctx, "rowid-asset-1"); err != nil {
			t.Fatal(err)
		}
		if err := repo.ReplaceAssetShots(ctx, "rowid-asset-1", "", []domain.AssetShot{
			{StartMS: 0, EndMS: 500, Description: "reprocessed shot"},
		}); err != nil {
			t.Fatal(err)
		}
	}

	assertCounts := func(label, countQuery string, want int) {
		var got int
		if err := repo.db.QueryRowContext(ctx, countQuery).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s=%d, want %d", label, got, want)
		}
	}
	assertCounts("asset_search rows", `SELECT COUNT(*) FROM asset_search`, len(assetIDs))
	assertCounts("asset_search_rowids rows", `SELECT COUNT(*) FROM asset_search_rowids`, len(assetIDs))
	assertCounts("asset_shot_search rows for reprocessed asset", `SELECT COUNT(*) FROM asset_shot_search WHERE asset_id='rowid-asset-1'`, 1)
	assertCounts("asset_shot_search_rowids rows for reprocessed asset", `SELECT COUNT(*) FROM asset_shot_search_rowids WHERE asset_id='rowid-asset-1'`, 1)
	// Two untouched assets still have their original two shots each, plus the
	// one remaining shot on the reprocessed asset.
	assertCounts("total asset_shot_search rows", `SELECT COUNT(*) FROM asset_shot_search`, 2*2+1)
	assertCounts("total asset_shot_search_rowids rows", `SELECT COUNT(*) FROM asset_shot_search_rowids`, 2*2+1)

	// Every FTS shadow-table row must have exactly one mapping row, and every
	// mapping row must point at a row id that still exists in the shadow
	// table -- i.e. the mapping tables never drift from what's actually
	// indexed.
	var unmappedSearchRows, danglingSearchMappings int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_search s WHERE NOT EXISTS (SELECT 1 FROM asset_search_rowids m WHERE m.search_rowid=s.rowid)`).Scan(&unmappedSearchRows); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_search_rowids m WHERE NOT EXISTS (SELECT 1 FROM asset_search s WHERE s.rowid=m.search_rowid)`).Scan(&danglingSearchMappings); err != nil {
		t.Fatal(err)
	}
	if unmappedSearchRows != 0 || danglingSearchMappings != 0 {
		t.Fatalf("asset_search mapping drift: unmapped=%d dangling=%d", unmappedSearchRows, danglingSearchMappings)
	}

	var unmappedShotRows, danglingShotMappings int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_shot_search s WHERE NOT EXISTS (SELECT 1 FROM asset_shot_search_rowids m WHERE m.search_rowid=s.rowid)`).Scan(&unmappedShotRows); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_shot_search_rowids m WHERE NOT EXISTS (SELECT 1 FROM asset_shot_search s WHERE s.rowid=m.search_rowid)`).Scan(&danglingShotMappings); err != nil {
		t.Fatal(err)
	}
	if unmappedShotRows != 0 || danglingShotMappings != 0 {
		t.Fatalf("asset_shot_search mapping drift: unmapped=%d dangling=%d", unmappedShotRows, danglingShotMappings)
	}

	// The FTS content itself must still be correct after all that churn.
	hits, err := repo.SearchShots(ctx, "reprocessed", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].AssetID != "rowid-asset-1" {
		t.Fatalf("reprocessed shot search=%+v", hits)
	}
}

// TestTagSearchNormalizedTagIndexIsUsable proves the fix for the unindexed
// normalized_tag lookup in Search(): idx_asset_tag_links_normalized lets a
// plain `WHERE normalized_tag=?` predicate seek instead of scanning
// asset_tag_links, and the UNION-based rewrite of the tag/canonical/alias
// lookup (needed because a three-way OR across LEFT-JOINed tables defeats
// the query planner's ability to push any single disjunct down to an index)
// drives every branch through an index too. It also checks the rewritten
// query still returns the same matches as the original OR-based one.
func TestTagSearchNormalizedTagIndexIsUsable(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "tag-search-plan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	isolated := explainQueryPlan(t, repo, `SELECT asset_id FROM asset_tag_links WHERE normalized_tag=?`, "x")
	if len(isolated) != 1 || !strings.Contains(isolated[0], "USING INDEX idx_asset_tag_links_normalized") {
		t.Fatalf("expected normalized_tag lookup to use the new index, got %v", isolated)
	}

	rewritten := explainQueryPlan(t, repo, `SELECT asset_id FROM (
SELECT l.asset_id FROM asset_tag_links l WHERE l.normalized_tag=?
UNION
SELECT l.asset_id FROM asset_tag_links l JOIN tag_catalog t ON t.id=l.canonical_tag_id WHERE t.canonical_name=?
UNION
SELECT l.asset_id FROM asset_tag_links l JOIN tag_aliases_v2 a ON a.canonical_tag_id=l.canonical_tag_id WHERE a.alias_normalized=?
)
LIMIT ?`, "x", "x", "x", 10)
	foundNormalizedSeek := false
	for _, detail := range rewritten {
		if strings.Contains(detail, "SEARCH l USING INDEX idx_asset_tag_links_normalized") {
			foundNormalizedSeek = true
		}
		if detail == "SCAN l USING INDEX sqlite_autoindex_asset_tag_links_1" {
			t.Fatalf("rewritten tag search still falls back to a full scan of asset_tag_links: %v", rewritten)
		}
	}
	if !foundNormalizedSeek {
		t.Fatalf("expected the normalized_tag branch of the rewritten search to seek via the new index, got %v", rewritten)
	}

	// End-to-end: the rewritten Search() query must still return the same
	// results as before (literal tag, canonical name, and alias all resolve
	// to the same asset).
	now := formatTime(time.Now())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('tag-root','/footage',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('tag-asset','fp',1,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES('tag-loc','tag-asset','tag-root','clip.mp4','/footage/clip.mp4',1,1,1,?)`, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.SyncAnalysisTags(ctx, "tag-asset", "run-tag", domain.StructuredAnalysis{SceneTags: []string{"城市夜景"}}); err != nil {
		t.Fatal(err)
	}
	unresolved, err := repo.ListUnresolvedTags(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateTagCurationRun(ctx, []domain.TagProposal{{
		ProposalType:  "create_canonical_with_aliases",
		CanonicalName: "urban_night",
		Payload: map[string]any{
			"canonical_name": "urban_night",
			"aliases":        []string{"城市夜景", "night_city"},
			"category":       "scene",
		},
		Confidence: 0.9, Reason: "index test fixture", AffectedAssets: 1,
	}}, len(unresolved), "fixture-index-test"); err != nil {
		t.Fatal(err)
	}
	proposals, err := repo.ListTagProposals(ctx, "pending", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 1 {
		t.Fatalf("proposals=%+v", proposals)
	}
	if err := repo.ReviewTagProposal(ctx, proposals[0].ID, "approve", "index test approval"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"urban_night", "城市夜景", "night_city"} {
		ids, err := repo.Search(ctx, query, 10)
		if err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
		if len(ids) != 1 || ids[0] != "tag-asset" {
			t.Fatalf("search %q=%v, want [tag-asset]", query, ids)
		}
	}
}
