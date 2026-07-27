package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ev/timingdex/internal/domain"
)

func TestAssetCollectionsPersistNamedNonSecretFilters(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "collections.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	saved, err := repo.SaveAssetCollection(ctx, domain.AssetCollection{
		Name:        "深圳夜景素材",
		Description: "城市宣传片候选",
		Filter: domain.AssetCollectionFilter{
			CapturedFrom: &from,
			CapturedTo:   &to,
			RegionLabel:  "中国 · 深圳",
			CameraModel:  "Sony FX3",
			SessionID:    "session-shenzhen",
			Status:       domain.ProcessingStatusReady,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID == "" || saved.CreatedAt.IsZero() || saved.UpdatedAt.IsZero() {
		t.Fatalf("saved collection lacks identity/timestamps: %+v", saved)
	}

	got, err := repo.GetAssetCollection(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Name != saved.Name || got.Filter.RegionLabel != "中国 · 深圳" || got.Filter.Status != domain.ProcessingStatusReady {
		t.Fatalf("got collection=%+v, want persisted filter", got)
	}
	if got.Filter.CapturedFrom == nil || !got.Filter.CapturedFrom.Equal(from) || got.Filter.CapturedTo == nil || !got.Filter.CapturedTo.Equal(to) {
		t.Fatalf("got date filter=%+v, want round-tripped dates", got.Filter)
	}

	collections, err := repo.ListAssetCollections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(collections) != 1 || collections[0].ID != saved.ID {
		t.Fatalf("collections=%+v, want one saved collection", collections)
	}
	updated, err := repo.SaveAssetCollection(ctx, domain.AssetCollection{
		ID:          saved.ID,
		Name:        saved.Name,
		Description: "已更新",
		Filter:      domain.AssetCollectionFilter{Status: domain.ProcessingStatusQueued},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.CreatedAt.Equal(saved.CreatedAt) || updated.Description != "已更新" || updated.Filter.Status != domain.ProcessingStatusQueued {
		t.Fatalf("updated collection=%+v, want stable created_at and replaced filter", updated)
	}
	collections, err = repo.ListAssetCollections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(collections) != 1 {
		t.Fatalf("updating collection created duplicate rows: %+v", collections)
	}
	var rawFilter string
	if err := repo.db.QueryRowContext(ctx, `SELECT filter_json FROM asset_collections WHERE id=?`, saved.ID).Scan(&rawFilter); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"api_key", "absolute_path", "latitude", "longitude"} {
		if containsInsensitive(rawFilter, forbidden) {
			t.Fatalf("collection filter persisted forbidden field %q: %s", forbidden, rawFilter)
		}
	}

	if err := repo.DeleteAssetCollection(ctx, saved.ID); err != nil {
		t.Fatal(err)
	}
	deleted, err := repo.GetAssetCollection(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != nil {
		t.Fatalf("deleted collection still returned: %+v", deleted)
	}
}

func TestCollectionCardsFilterByProcessingStatusWithoutPrivateBrowseFields(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "collection-assets.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	stamp := formatTime(time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC))
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root-1','/nas/library',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	fixtures := []struct {
		id, state, jobState string
	}{
		{id: "ready-1", state: "discovered"},
		{id: "queued-1", state: "discovered", jobState: "pending"},
		{id: "failed-1", state: "discovered", jobState: "failed"},
		{id: "missing-1", state: "missing"},
	}
	for _, fixture := range fixtures {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,?,?,?)`, fixture.id, fixture.id, fixture.state, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,?, ?, ?,1,1,1,?)`, "loc-"+fixture.id, fixture.id, "root-1", fixture.id+".mov", "/nas/library/"+fixture.id+".mov", stamp); err != nil {
			t.Fatal(err)
		}
		captured := time.Date(2026, 7, 27, 8, 0, 0, 0, time.UTC)
		if err := repo.SaveMediaMetadata(ctx, fixture.id, domain.MediaMetadata{CapturedAt: &captured, CameraModel: "Sony FX3"}, "fixture"); err != nil {
			t.Fatal(err)
		}
		if fixture.id == "ready-1" {
			if err := repo.SaveArtifact(ctx, domain.DerivedArtifact{ID: "artifact-ready", AssetID: fixture.id, Type: "proxy", ProfileHash: "proxy-v1", LocalPath: "/derived/ready.mp4"}); err != nil {
				t.Fatal(err)
			}
		}
		if fixture.jobState != "" {
			if _, err := repo.db.ExecContext(ctx, `INSERT INTO jobs(id,asset_id,job_type,state,attempt_count,max_attempts,run_after,input_hash,created_at,updated_at) VALUES(?,?,?,?,?,?,?, ?,?,?)`, "job-"+fixture.id, fixture.id, "analyze", fixture.jobState, 3, 3, stamp, "input-"+fixture.id, stamp, stamp); err != nil {
				t.Fatal(err)
			}
		}
	}

	collection, err := repo.SaveAssetCollection(ctx, domain.AssetCollection{
		Name: "待处理素材",
		Filter: domain.AssetCollectionFilter{
			CameraModel: "Sony FX3",
			Status:      domain.ProcessingStatusQueued,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cards, err := repo.ListAssetCardsInCollection(ctx, collection.ID, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 || cards[0].ID != "queued-1" || cards[0].ProcessingStatus != domain.ProcessingStatusQueued {
		t.Fatalf("collection cards=%+v, want queued-1 only", cards)
	}
	if cards[0].Filename != "queued-1.mov" || cards[0].ThumbnailURL != "" || cards[0].ProxyURL != "" {
		t.Fatalf("unexpected browse projection=%+v", cards[0])
	}

	summary, err := repo.GetAssetProcessingSummary(ctx, domain.AssetCollectionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total != 4 || summary.ByStatus[domain.ProcessingStatusReady] != 1 || summary.ByStatus[domain.ProcessingStatusQueued] != 1 || summary.ByStatus[domain.ProcessingStatusFailed] != 1 || summary.ByStatus[domain.ProcessingStatusMissing] != 1 {
		t.Fatalf("summary=%+v, want all four operational statuses", summary)
	}
}

func containsInsensitive(value, needle string) bool {
	return len(value) >= len(needle) && strings.Contains(strings.ToLower(value), strings.ToLower(needle))
}
