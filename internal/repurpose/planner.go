package repurpose

import (
	"sort"
	"strings"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

func HeuristicDraft(brief domain.RepurposeBrief) domain.RepurposePlanDraft {
	duration := brief.DurationMS
	if duration <= 0 {
		duration = 30000
	}
	text := strings.ToLower(strings.TrimSpace(brief.Brief))
	cityQuery := "城市"
	if strings.Contains(text, "city") {
		cityQuery = "city"
	}
	peopleQuery := "人"
	if strings.Contains(text, "people") || strings.Contains(text, "person") {
		peopleQuery = "person"
	}
	moodQuery := "夜景"
	if strings.Contains(text, "rain") || strings.Contains(text, "雨") {
		moodQuery = "雨"
	}
	return domain.RepurposePlanDraft{
		Title: strings.TrimSpace(brief.Brief),
		Sections: []domain.MaterialNeed{
			{Role: "opening", Query: cityQuery, DurationMS: duration / 6, Required: true, Rationale: "先建立地点和整体氛围"},
			{Role: "human_activity", Query: peopleQuery, DurationMS: duration / 3, Required: true, Rationale: "补充人物与生活感"},
			{Role: "mood", Query: moodQuery, DurationMS: duration / 3, Required: false, Rationale: "增加情绪和视觉记忆点"},
			{Role: "ending", Query: cityQuery, DurationMS: duration - duration/6 - duration/3 - duration/3, Required: true, Rationale: "用城市画面收束"},
		},
	}
}

func ComposePlan(brief domain.RepurposeBrief, draft domain.RepurposePlanDraft, hits map[string][]domain.ShotSearchResult, provider, model string) domain.RepurposePlan {
	duration := brief.DurationMS
	if duration <= 0 {
		for _, section := range draft.Sections {
			duration += section.DurationMS
		}
	}
	title := strings.TrimSpace(draft.Title)
	if title == "" {
		title = strings.TrimSpace(brief.Brief)
	}
	maxCandidates := brief.MaxCandidates
	if maxCandidates <= 0 {
		maxCandidates = 3
	}
	plan := domain.RepurposePlan{
		Brief: brief.Brief, DurationMS: duration, Style: brief.Style, Audience: brief.Audience,
		Title: title, Status: "draft", Provider: provider, Model: model,
		Sections: make([]domain.PlanSection, 0, len(draft.Sections)),
	}
	used := map[string]bool{}
	for _, need := range draft.Sections {
		section := domain.PlanSection{Role: need.Role, Query: need.Query, DurationMS: need.DurationMS, Required: need.Required, Rationale: need.Rationale}
		candidates := append([]domain.ShotSearchResult(nil), hits[need.Query]...)
		sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Score > candidates[j].Score })
		for _, hit := range candidates {
			if hit.ID == "" || used[hit.ID] || len(section.Candidates) >= maxCandidates {
				continue
			}
			used[hit.ID] = true
			section.Candidates = append(section.Candidates, planCandidate(hit, false))
		}
		// Unique candidates are preferred across sections. A required section
		// must not disappear merely because its only valid shot was selected for
		// an earlier role; retain the provenance and mark that deliberate reuse.
		if len(section.Candidates) == 0 && section.Required {
			for _, hit := range candidates {
				if hit.ID != "" {
					section.Candidates = append(section.Candidates, planCandidate(hit, used[hit.ID]))
					break
				}
			}
		}
		if len(section.Candidates) == 0 && section.Required {
			plan.MissingNeeds = append(plan.MissingNeeds, need.Role+": "+need.Query)
		}
		plan.Sections = append(plan.Sections, section)
	}
	return plan
}

func planCandidate(hit domain.ShotSearchResult, reused bool) domain.PlanCandidate {
	reasons := []string{}
	if hit.Description != "" {
		reasons = append(reasons, hit.Description)
	}
	if len(hit.Tags) > 0 {
		reasons = append(reasons, "标签："+strings.Join(hit.Tags, "、"))
	}
	return domain.PlanCandidate{ShotID: hit.ID, AssetID: hit.AssetID, StartMS: hit.StartMS, EndMS: hit.EndMS, Score: hit.Score, Reused: reused, Reasons: reasons}
}
