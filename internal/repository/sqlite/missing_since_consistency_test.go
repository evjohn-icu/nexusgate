package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

// TestMissingSinceParseConsistencyAcrossListAndDetail ensures ListAssets and
// GetAssetDetail agree on dirty missing_since values: both must return nil
// (not &time.Time{}) when the stored text is not a valid RFC3339Nano timestamp.
func TestMissingSinceParseConsistencyAcrossListAndDetail(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "missing-consistency.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	now := formatTime(time.Now().UTC())

	// Insert an asset with a well-formed missing_since (control) and one with a
	// deliberately unparseable value (dirty data from pre-constraint era).
	if _, err := repo.db.ExecContext(ctx,
		`INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at,missing_since) VALUES(?,?,100,'discovered',?,?,?)`,
		"asset-clean", "fp-clean", now, now, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx,
		`INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at,missing_since) VALUES(?,?,100,'missing',?,?,?)`,
		"asset-dirty", "fp-dirty", now, now, "not-a-timestamp",
	); err != nil {
		t.Fatal(err)
	}
	// The library root must exist for the location FK.
	if _, err := repo.db.ExecContext(ctx,
		`INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root-x','/footage',?,?)`, now, now,
	); err != nil {
		t.Fatal(err)
	}
	// GetAssetDetail needs a primary location.
	for _, aid := range []string{"asset-clean", "asset-dirty"} {
		if _, err := repo.db.ExecContext(ctx,
			`INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,?,?,?,1,1,1,?)`,
			"loc-"+aid, aid, "root-x", aid+".mp4", "/footage/"+aid+".mp4", now,
		); err != nil {
			t.Fatal(err)
		}
	}

	// ListAssets
	assets, err := repo.ListAssets(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 2 {
		t.Fatalf("expected 2 assets, got %d", len(assets))
	}
	var cleanAsset, dirtyAsset *domain.Asset
	for i := range assets {
		switch assets[i].ID {
		case "asset-clean":
			cleanAsset = &assets[i]
		case "asset-dirty":
			dirtyAsset = &assets[i]
		}
	}
	if cleanAsset == nil || dirtyAsset == nil {
		t.Fatal("both assets must be present in ListAssets result")
	}
	if cleanAsset.MissingSince == nil {
		t.Fatal("clean asset: expected non-nil MissingSince (valid timestamp)")
	}
	if dirtyAsset.MissingSince != nil {
		t.Fatalf("dirty asset: expected nil MissingSince for unparseable value, got %v", *dirtyAsset.MissingSince)
	}

	// GetAssetDetail — must agree with ListAssets.
	cleanDetail, err := repo.GetAssetDetail(ctx, "asset-clean")
	if err != nil || cleanDetail == nil {
		t.Fatalf("GetAssetDetail clean: err=%v, detail=%v", err, cleanDetail)
	}
	if cleanDetail.Asset.MissingSince == nil {
		t.Fatal("GetAssetDetail clean: expected non-nil MissingSince")
	}

	dirtyDetail, err := repo.GetAssetDetail(ctx, "asset-dirty")
	if err != nil || dirtyDetail == nil {
		t.Fatalf("GetAssetDetail dirty: err=%v, detail=%v", err, dirtyDetail)
	}
	if dirtyDetail.Asset.MissingSince != nil {
		t.Fatalf("GetAssetDetail dirty: expected nil MissingSince for unparseable value, got %v", *dirtyDetail.Asset.MissingSince)
	}
}
