package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ev/timingdex/internal/domain"
)

func TestRepurposePlanPersistsStructuredRecommendations(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "timingdex-repurpose.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	want := domain.RepurposePlan{
		Brief:      "深圳城市宣传视频",
		Title:      "深圳城市生活",
		DurationMS: 30000,
		Status:     "draft",
		Provider:   "deterministic",
		Model:      "heuristic-v1",
		Sections:   []domain.PlanSection{{Role: "opening", Query: "urban_night", DurationMS: 5000, Candidates: []domain.PlanCandidate{{ShotID: "shot-1", AssetID: "asset-1", StartMS: 12000, EndMS: 18000, Score: 0.93}}}},
	}
	saved, err := repo.SaveRepurposePlan(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID == "" {
		t.Fatal("saved plan has no id")
	}
	got, err := repo.GetRepurposePlan(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Title != want.Title || len(got.Sections) != 1 || got.Sections[0].Candidates[0].ShotID != "shot-1" {
		t.Fatalf("loaded plan=%+v", got)
	}
}

func TestRepurposePlanRevisionsPreserveHistoryAndApprovedPlanIsImmutable(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "timingdex-revision.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	plan, err := repo.SaveRepurposePlan(ctx, domain.RepurposePlan{Brief: "深圳城市宣传", DurationMS: 30000, Title: "深圳", Status: "draft", Provider: "deterministic", Model: "heuristic-v1", Sections: []domain.PlanSection{{Role: "opening", Query: "city", DurationMS: 5000, Required: true}}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := repo.SaveRepurposePlanRevision(ctx, plan, "initial proposal")
	if err != nil || first.Revision != 1 || first.State != "draft" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	plan.Sections[0].Rationale = "editor adjusted"
	second, err := repo.SaveRepurposePlanRevision(ctx, plan, "opening refined")
	if err != nil || second.Revision != 2 {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	history, err := repo.ListRepurposePlanRevisions(ctx, plan.ID)
	if err != nil || len(history) != 2 || history[0].EditorNote != "opening refined" || history[1].EditorNote != "initial proposal" {
		t.Fatalf("history=%+v err=%v", history, err)
	}
	approved, err := repo.ApproveRepurposePlanRevision(ctx, plan.ID, 2)
	if err != nil || approved.State != "approved" || approved.Plan.Status != "approved" {
		t.Fatalf("approved=%+v err=%v", approved, err)
	}
	if _, err := repo.SaveRepurposePlanRevision(ctx, plan, "must fail"); err == nil {
		t.Fatal("expected approved plan to reject further revisions")
	}
}
