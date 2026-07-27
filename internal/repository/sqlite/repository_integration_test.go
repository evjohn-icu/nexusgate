package sqlite

import (
	"context"
	"path/filepath"
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
