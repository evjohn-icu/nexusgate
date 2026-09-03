package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

// insertPagedAsset seeds the minimal assets row the FK-referencing tables in
// the pagination tests need, with an explicit first_seen_at so the asset-card
// ordering is deterministic.
func insertPagedAsset(t *testing.T, repo *Repository, id string, firstSeen time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'discovered',?,?)`, id, id+"-fp", formatTime(firstSeen), formatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
}

// walkPaged is the shared assertion for every Paged* method: fetch pages of
// `limit` at increasing offsets until hasMore is false, concatenate, and prove
// there are no duplicate or missing IDs.
func walkPaged[T any](t *testing.T, limit, total int, label string, idOf func(T) string, next func(offset int) ([]T, bool, error)) {
	t.Helper()
	var collected []string
	offset := 0
	pages := 0
	for {
		items, hasMore, err := next(offset)
		if err != nil {
			t.Fatalf("%s page %d: %v", label, pages, err)
		}
		if len(items) > limit {
			t.Fatalf("%s page %d returned %d items, want at most %d", label, pages, len(items), limit)
		}
		for _, item := range items {
			collected = append(collected, idOf(item))
		}
		pages++
		if !hasMore {
			break
		}
		if pages > total+2 {
			t.Fatalf("%s never reached hasMore=false (collected %d)", label, len(collected))
		}
		offset += limit
	}
	if len(collected) != total {
		t.Fatalf("%s collected %d ids, want %d", label, len(collected), total)
	}
	seen := make(map[string]bool, total)
	for _, id := range collected {
		if seen[id] {
			t.Fatalf("%s returned duplicate id %q across pages", label, id)
		}
		seen[id] = true
	}
}

func TestPagedJobsDeterministicTieBreakAndProbe(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "paged-jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const total = 7
	for i := range total {
		id := fmt.Sprintf("asset-%d", i)
		insertPagedAsset(t, repo, id, time.Now())
		if err := repo.EnqueueJob(ctx, id, domain.JobProbe, fmt.Sprintf("input-%d", i), 0); err != nil {
			t.Fatal(err)
		}
	}
	walkPaged(t, 3, total, "jobs", func(j domain.Job) string { return j.ID }, func(offset int) ([]domain.Job, bool, error) {
		return repo.PagedJobs(ctx, 3, offset)
	})

	// A page whose size exactly matches the total has no more rows.
	exact, hasMore, err := repo.PagedJobs(ctx, total, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(exact) != total || hasMore {
		t.Fatalf("exact-size page len=%d hasMore=%v, want len=%d hasMore=false", len(exact), hasMore, total)
	}
	// A page past the end is empty and not "more".
	empty, hasMore, err := repo.PagedJobs(ctx, 3, total*2)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 || hasMore {
		t.Fatalf("past-end page len=%d hasMore=%v, want empty hasMore=false", len(empty), hasMore)
	}
}

func TestPagedRepurposePlansReturnsEveryPlanOnce(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "paged-plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const total = 7
	for i := range total {
		if _, err := repo.SaveRepurposePlan(ctx, domain.RepurposePlan{Brief: fmt.Sprintf("brief %d", i), DurationMS: 1000, Title: fmt.Sprintf("plan %d", i), Status: "draft", Provider: "deterministic", Model: "heuristic-v1", Sections: []domain.PlanSection{{Role: "opening", Query: "x", DurationMS: 1000, Required: true}}}); err != nil {
			t.Fatal(err)
		}
	}
	walkPaged(t, 3, total, "repurpose plans", func(p domain.RepurposePlanSummary) string { return p.ID }, func(offset int) ([]domain.RepurposePlanSummary, bool, error) {
		return repo.PagedRepurposePlans(ctx, "all", 3, offset)
	})

	drafts, hasMore, err := repo.PagedRepurposePlans(ctx, "draft", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != total || hasMore {
		t.Fatalf("draft page len=%d hasMore=%v, want %d false", len(drafts), hasMore, total)
	}
}

func TestPagedUnresolvedTagsReturnsEveryTagOnce(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "paged-unresolved.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	insertPagedAsset(t, repo, "asset-tags", time.Now())
	now := formatTime(time.Now())
	const total = 7
	for i := range total {
		tag := fmt.Sprintf("unresolved-%d", i)
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_tag_links(asset_id,raw_tag,normalized_tag,tag_type,source,created_at,updated_at) VALUES(?,?,?,?,'ai',?,?)`, "asset-tags", tag, tag, "topic", now, now); err != nil {
			t.Fatal(err)
		}
	}
	walkPaged(t, 3, total, "unresolved tags", func(u domain.UnresolvedTag) string { return u.NormalizedTag }, func(offset int) ([]domain.UnresolvedTag, bool, error) {
		return repo.PagedUnresolvedTags(ctx, 3, offset)
	})
}

func TestPagedTagProposalsReturnsEveryProposalOnce(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "paged-proposals.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO tag_curation_runs(id,state,strategy,input_revision,stats_json,created_at,finished_at) VALUES('run-paged','completed','test','rev','{}',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	const total = 7
	for i := range total {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO tag_change_proposals(id,run_id,state,proposal_type,canonical_name,payload_json,confidence,reason,affected_assets,created_at) VALUES(?,?,'pending','merge',?,?,1.0,?,1,?)`, fmt.Sprintf("prop-%d", i), "run-paged", fmt.Sprintf("tag-%d", i), "{}", fmt.Sprintf("reason-%d", i), now); err != nil {
			t.Fatal(err)
		}
	}
	walkPaged(t, 3, total, "tag proposals", func(p domain.TagProposal) string { return p.ID }, func(offset int) ([]domain.TagProposal, bool, error) {
		return repo.PagedTagProposals(ctx, "", 3, offset)
	})

	// The state filter narrows the same paged path.
	pending, hasMore, err := repo.PagedTagProposals(ctx, "pending", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != total || hasMore {
		t.Fatalf("pending page len=%d hasMore=%v, want %d false", len(pending), hasMore, total)
	}
}

func TestPagedShootSessionsReturnsEverySessionOnce(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "paged-sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const total = 7
	for i := range total {
		if err := repo.SaveShootSession(ctx, domain.ShootSession{ID: fmt.Sprintf("session-%d", i), Title: fmt.Sprintf("S %d", i), State: "automatic"}); err != nil {
			t.Fatal(err)
		}
	}
	walkPaged(t, 3, total, "shoot sessions", func(s domain.ShootSession) string { return s.ID }, func(offset int) ([]domain.ShootSession, bool, error) {
		return repo.PagedShootSessions(ctx, domain.ShootSessionFilter{Limit: 3, Offset: offset})
	})
}

func TestPagedAssetCardsFilteredReturnsEveryCardOnce(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "paged-assets.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const total = 7
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range total {
		insertPagedAsset(t, repo, fmt.Sprintf("card-%d", i), base.Add(time.Duration(i)*time.Minute))
	}
	walkPaged(t, 3, total, "asset cards", func(c domain.AssetCard) string { return c.ID }, func(offset int) ([]domain.AssetCard, bool, error) {
		return repo.PagedAssetCardsFiltered(ctx, domain.AssetCardFilter{Limit: 3, Offset: offset})
	})
}

func TestAssetExists(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "asset-exists.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	insertPagedAsset(t, repo, "present-asset", time.Now())
	found, err := repo.AssetExists(ctx, "present-asset")
	if err != nil || !found {
		t.Fatalf("AssetExists(present) = %v, %v; want true, nil", found, err)
	}
	missing, err := repo.AssetExists(ctx, "no-such-asset")
	if err != nil || missing {
		t.Fatalf("AssetExists(missing) = %v, %v; want false, nil", missing, err)
	}
}

func TestCollectionExists(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "collection-exists.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_collections(id,name,description,filter_json,created_at,updated_at) VALUES('present-collection','Basket','','{}',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	found, err := repo.CollectionExists(ctx, "present-collection")
	if err != nil || !found {
		t.Fatalf("CollectionExists(present) = %v, %v; want true, nil", found, err)
	}
	missing, err := repo.CollectionExists(ctx, "no-such-collection")
	if err != nil || missing {
		t.Fatalf("CollectionExists(missing) = %v, %v; want false, nil", missing, err)
	}
}
