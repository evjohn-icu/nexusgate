package repurpose

import (
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

func TestComposePlanPlacesShotCandidatesIntoBriefSections(t *testing.T) {
	brief := domain.RepurposeBrief{Brief: "深圳城市宣传视频", DurationMS: 30000}
	draft := domain.RepurposePlanDraft{
		Title: "深圳城市生活",
		Sections: []domain.MaterialNeed{
			{Role: "opening", Query: "urban_night", DurationMS: 5000, Required: true},
			{Role: "human_activity", Query: "street", DurationMS: 8000, Required: true},
		},
	}
	hits := map[string][]domain.ShotSearchResult{
		"urban_night": {{AssetShot: domain.AssetShot{ID: "shot-1", AssetID: "asset-1", StartMS: 12000, EndMS: 18000, Description: "城市夜景", Tags: []string{"urban_night"}}, Score: 0.93}},
		"street":      {{AssetShot: domain.AssetShot{ID: "shot-2", AssetID: "asset-2", StartMS: 2000, EndMS: 7000, Description: "街道人流", Tags: []string{"street"}}, Score: 0.88}},
	}

	plan := ComposePlan(brief, draft, hits, "deterministic", "heuristic-v1")
	if plan.Title != "深圳城市生活" || len(plan.Sections) != 2 {
		t.Fatalf("plan=%+v", plan)
	}
	if len(plan.Sections[0].Candidates) != 1 || plan.Sections[0].Candidates[0].ShotID != "shot-1" {
		t.Fatalf("opening candidates=%+v", plan.Sections[0].Candidates)
	}
	if len(plan.MissingNeeds) != 0 || plan.Status != "draft" {
		t.Fatalf("plan completeness=%+v", plan)
	}
}

func TestHeuristicDraftReportsRequiredNeedsWhenBriefHasNoMatches(t *testing.T) {
	draft := HeuristicDraft(domain.RepurposeBrief{Brief: "城市宣传视频", DurationMS: 30000})
	if len(draft.Sections) < 3 {
		t.Fatalf("heuristic sections=%+v", draft.Sections)
	}
	if draft.Sections[0].Role != "opening" || draft.Sections[0].Query == "" {
		t.Fatalf("heuristic opening=%+v", draft.Sections[0])
	}
}

func TestComposePlanMarksRequiredEndingReuseInsteadOfLeavingItEmpty(t *testing.T) {
	brief := domain.RepurposeBrief{Brief: "城市宣传视频", DurationMS: 10000, MaxCandidates: 1}
	draft := domain.RepurposePlanDraft{Sections: []domain.MaterialNeed{
		{Role: "opening", Query: "city", DurationMS: 5000, Required: true},
		{Role: "ending", Query: "city", DurationMS: 5000, Required: true},
	}}
	hits := map[string][]domain.ShotSearchResult{
		"city": {{AssetShot: domain.AssetShot{ID: "only-city-shot", AssetID: "asset-city", StartMS: 0, EndMS: 5000, Description: "城市天际线"}, Score: 0.9}},
	}

	plan := ComposePlan(brief, draft, hits, "deterministic", "heuristic-v1")
	if len(plan.MissingNeeds) != 0 || len(plan.Sections[1].Candidates) != 1 {
		t.Fatalf("required ending was starved: %+v", plan)
	}
	if !plan.Sections[1].Candidates[0].Reused || plan.Sections[1].Candidates[0].ShotID != "only-city-shot" {
		t.Fatalf("ending reuse is not explicit: %+v", plan.Sections[1].Candidates)
	}
}
