package domain

import "time"

type RepurposeBrief struct {
	Brief         string `json:"brief"`
	DurationMS    int64  `json:"duration_ms,omitempty"`
	Style         string `json:"style,omitempty"`
	Audience      string `json:"audience,omitempty"`
	MaxCandidates int    `json:"max_candidates,omitempty"`
}

type MaterialNeed struct {
	Role       string `json:"role"`
	Query      string `json:"query"`
	DurationMS int64  `json:"duration_ms"`
	Required   bool   `json:"required"`
	Rationale  string `json:"rationale,omitempty"`
}

type RepurposePlanDraft struct {
	Title    string         `json:"title"`
	Sections []MaterialNeed `json:"sections"`
}

type PlanCandidate struct {
	ShotID  string   `json:"shot_id"`
	AssetID string   `json:"asset_id"`
	StartMS int64    `json:"start_ms"`
	EndMS   int64    `json:"end_ms"`
	Score   float64  `json:"score"`
	Reused  bool     `json:"reused,omitempty"`
	Reasons []string `json:"reasons,omitempty"`
}

type PlanSection struct {
	Role            string          `json:"role"`
	Query           string          `json:"query"`
	DurationMS      int64           `json:"duration_ms"`
	Required        bool            `json:"required"`
	Rationale       string          `json:"rationale,omitempty"`
	Candidates      []PlanCandidate `json:"candidates,omitempty"`
	SelectedShotID  string          `json:"selected_shot_id,omitempty"`
	Locked          bool            `json:"locked,omitempty"`
	Unlock          bool            `json:"unlock,omitempty"`
	ExcludedShotIDs []string        `json:"excluded_shot_ids,omitempty"`
}

type RepurposePlan struct {
	ID           string        `json:"id"`
	Brief        string        `json:"brief"`
	DurationMS   int64         `json:"duration_ms"`
	Style        string        `json:"style,omitempty"`
	Audience     string        `json:"audience,omitempty"`
	Title        string        `json:"title"`
	Status       string        `json:"status"`
	Provider     string        `json:"provider"`
	Model        string        `json:"model"`
	Sections     []PlanSection `json:"sections"`
	MissingNeeds []string      `json:"missing_needs,omitempty"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
}

// RepurposePlanRevision is an immutable snapshot of an editor-controlled
// iteration. Approved plans are locked so v1.0 hand-off can rely on a stable
// selection without allowing automatic media edits.
type RepurposePlanRevision struct {
	ID         string        `json:"id"`
	PlanID     string        `json:"plan_id"`
	Revision   int           `json:"revision"`
	State      string        `json:"state"`
	Plan       RepurposePlan `json:"plan"`
	EditorNote string        `json:"editor_note,omitempty"`
	CreatedAt  time.Time     `json:"created_at"`
	ApprovedAt *time.Time    `json:"approved_at,omitempty"`
}
