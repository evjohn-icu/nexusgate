package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

func TestRepurposePlanPersistsStructuredRecommendations(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "nexusgate-repurpose.db"))
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
	repo, err := Open(filepath.Join(t.TempDir(), "nexusgate-revision.db"))
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

// TestRepurposePlanWritesWrapDomainSentinels pins the human-approval
// boundary's five refusals to errors.Is against the domain sentinels, driven
// through the repository directly with no service layer above it.
//
// It belongs here and not in internal/app because internal/app's tests run
// against in-memory fakes that enforce no schema constraint: a fake can hand
// back whatever error its author wrote, so a green app test proves nothing
// about what SQLite actually refuses or about which sentinel the real write
// wraps. These checks run inside the repository's own transaction and are the
// only ones that cannot be raced, which is what makes them the enforcement
// and this the place to assert on them.
func TestRepurposePlanWritesWrapDomainSentinels(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "repurpose-sentinels.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// A plan that does not exist: the revision write finds no row to revise.
	if _, err := repo.SaveRepurposePlanRevision(ctx, domain.RepurposePlan{ID: "no-such-plan"}, "orphan"); !errors.Is(err, domain.ErrPlanNotFound) {
		t.Fatalf("SaveRepurposePlanRevision on a missing plan: want errors.Is(err, domain.ErrPlanNotFound); got %v", err)
	}

	plan, err := repo.SaveRepurposePlan(ctx, domain.RepurposePlan{Brief: "sentinel coverage", DurationMS: 30000, Title: "sentinels", Status: "draft", Provider: "deterministic", Model: "heuristic-v1", Sections: []domain.PlanSection{{Role: "opening", Query: "city", DurationMS: 5000, Required: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SaveRepurposePlanRevision(ctx, plan, "rev 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SaveRepurposePlanRevision(ctx, plan, "rev 2"); err != nil {
		t.Fatal(err)
	}

	// A revision number the plan does not have.
	if _, err := repo.ApproveRepurposePlanRevision(ctx, plan.ID, 99); !errors.Is(err, domain.ErrPlanRevisionNotFound) {
		t.Fatalf("approving revision 99: want errors.Is(err, domain.ErrPlanRevisionNotFound); got %v", err)
	}

	// Revision 1 is a draft, but revision 2 superseded it: approving it would
	// resurrect a selection the operator already moved past.
	if _, err := repo.ApproveRepurposePlanRevision(ctx, plan.ID, 1); !errors.Is(err, domain.ErrPlanRevisionNotLatest) {
		t.Fatalf("approving the superseded revision 1: want errors.Is(err, domain.ErrPlanRevisionNotLatest); got %v", err)
	}

	if _, err := repo.ApproveRepurposePlanRevision(ctx, plan.ID, 2); err != nil {
		t.Fatal(err)
	}

	// Revision 2 is approved now, so approving it again is acting on state
	// that already moved -- not an idempotent repeat.
	if _, err := repo.ApproveRepurposePlanRevision(ctx, plan.ID, 2); !errors.Is(err, domain.ErrPlanRevisionNotDraft) {
		t.Fatalf("re-approving revision 2: want errors.Is(err, domain.ErrPlanRevisionNotDraft); got %v", err)
	}

	// Approval was the last write the plan accepts, at both write sites.
	if _, err := repo.SaveRepurposePlanRevision(ctx, plan, "after approval"); !errors.Is(err, domain.ErrPlanImmutable) {
		t.Fatalf("revising an approved plan: want errors.Is(err, domain.ErrPlanImmutable); got %v", err)
	}
	if _, err := repo.SaveRepurposePlan(ctx, plan); !errors.Is(err, domain.ErrPlanImmutable) {
		t.Fatalf("overwriting an approved plan: want errors.Is(err, domain.ErrPlanImmutable); got %v", err)
	}
}

// TestListRepurposePlansReturnsSummaries pins the plan-inbox projection: it
// lists every plan as a summary (never a candidate payload), carries revision
// counts and latest revision, and honours the draft/approved filter — so a
// human can discover and deep-link a plan an agent drafted.
func TestListRepurposePlansReturnsSummaries(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "repurpose-list.db"))
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
	if _, err := repo.SaveRepurposePlanRevision(ctx, plan, "开场用雨夜航拍"); err != nil {
		t.Fatal(err)
	}
	// Approval is a write only ApproveRepurposePlanRevision performs; the
	// generic save API never persists approved status (it writes draft), so
	// the approved summary here is produced through the real approval path.
	approvedPlan, err := repo.SaveRepurposePlan(ctx, domain.RepurposePlan{Brief: "海边空镜", DurationMS: 10000, Title: "海边", Status: "draft", Provider: "deterministic", Model: "heuristic-v1", Sections: []domain.PlanSection{{Role: "ending", Query: "sea", DurationMS: 5000, Required: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SaveRepurposePlanRevision(ctx, approvedPlan, "ending locked"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ApproveRepurposePlanRevision(ctx, approvedPlan.ID, 1); err != nil {
		t.Fatal(err)
	}

	all, err := repo.ListRepurposePlans(ctx, "all", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("all summaries = %d, want 2", len(all))
	}
	var draftSum, approvedSum domain.RepurposePlanSummary
	for _, s := range all {
		switch s.Title {
		case "深圳":
			draftSum = s
		case "海边":
			approvedSum = s
		}
	}
	if draftSum.ID == "" || approvedSum.ID == "" {
		t.Fatalf("summaries = %+v, want both plans present", all)
	}
	if draftSum.Status != "draft" || draftSum.RevisionCount != 1 || draftSum.LatestRevision != 1 {
		t.Fatalf("draft summary = %+v, want 1 revision, status draft", draftSum)
	}
	if approvedSum.Status != "approved" || approvedSum.RevisionCount != 1 || approvedSum.LatestRevision != 1 {
		t.Fatalf("approved summary = %+v, want 1 revision, status approved", approvedSum)
	}
	if draftSum.DurationMS != 30000 || draftSum.Brief == "" {
		t.Fatalf("draft summary = %+v, want duration and brief", draftSum)
	}

	drafts, err := repo.ListRepurposePlans(ctx, "draft", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 1 || drafts[0].Status != "draft" || drafts[0].ID != draftSum.ID {
		t.Fatalf("draft filter = %+v, want only the draft", drafts)
	}
	approved, err := repo.ListRepurposePlans(ctx, "approved", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(approved) != 1 || approved[0].Status != "approved" {
		t.Fatalf("approved filter = %+v, want only the approved plan", approved)
	}
}

// TestSaveRepurposePlanNeverWritesApprovedStatus pins the generic save's
// status rule: approval is a human decision only ApproveRepurposePlanRevision
// performs, so a caller-supplied approved status is stored as draft on a new
// plan, and an existing draft stays draft no matter what the payload claims.
func TestSaveRepurposePlanNeverWritesApprovedStatus(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "repurpose-status.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	sections := []domain.PlanSection{{Role: "opening", Query: "city", DurationMS: 5000, Required: true}}
	saved, err := repo.SaveRepurposePlan(ctx, domain.RepurposePlan{Brief: "claimed approved", DurationMS: 30000, Title: "status", Status: "approved", Provider: "deterministic", Model: "heuristic-v1", Sections: sections})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != "draft" {
		t.Fatalf("returned status=%q, want draft", saved.Status)
	}
	var status string
	if err := repo.db.QueryRowContext(ctx, `SELECT status FROM repurpose_plans WHERE id=?`, saved.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "draft" {
		t.Fatalf("stored status=%q, want the generic save to write draft", status)
	}

	// Re-saving the same draft with an approved claim keeps it a draft.
	if _, err := repo.SaveRepurposePlan(ctx, domain.RepurposePlan{ID: saved.ID, Brief: "claimed approved again", DurationMS: 30000, Title: "status", Status: "approved", Provider: "deterministic", Model: "heuristic-v1", Sections: sections}); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT status FROM repurpose_plans WHERE id=?`, saved.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "draft" {
		t.Fatalf("stored status after re-save=%q, want draft", status)
	}
}

// TestSaveRepurposePlanRevisionRollsBackOnFailedRevisionInsert drives the
// no-partial-mutation guarantee: the conditional plan update and the revision
// snapshot insert share one transaction, so a revision insert that fails must
// roll the plan row back to its previous content. The trigger stands in for
// any storage-level failure of the snapshot write.
func TestSaveRepurposePlanRevisionRollsBackOnFailedRevisionInsert(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "repurpose-rollback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	plan, err := repo.SaveRepurposePlan(ctx, domain.RepurposePlan{Brief: "before failure", DurationMS: 30000, Title: "rollback", Provider: "deterministic", Model: "heuristic-v1", Sections: []domain.PlanSection{{Role: "opening", Query: "city", DurationMS: 5000, Required: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SaveRepurposePlanRevision(ctx, plan, "initial"); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := repo.db.QueryRowContext(ctx, `SELECT plan_json FROM repurpose_plans WHERE id=?`, plan.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.db.ExecContext(ctx, `CREATE TRIGGER fail_revision_insert BEFORE INSERT ON repurpose_plan_revisions BEGIN SELECT RAISE(ABORT, 'fixture insert failure'); END`); err != nil {
		t.Fatal(err)
	}
	failed := plan
	failed.Brief = "brief that must not survive"
	if _, err := repo.SaveRepurposePlanRevision(ctx, failed, "doomed"); err == nil {
		t.Fatal("expected the failing revision insert to fail the whole write")
	}
	if _, err := repo.db.ExecContext(ctx, `DROP TRIGGER fail_revision_insert`); err != nil {
		t.Fatal(err)
	}

	var after string
	if err := repo.db.QueryRowContext(ctx, `SELECT plan_json FROM repurpose_plans WHERE id=?`, plan.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("failed revision insert mutated the plan: before=%s after=%s", before, after)
	}
	history, err := repo.ListRepurposePlanRevisions(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Fatalf("revisions=%d, want only the pre-failure revision", len(history))
	}
	// The same revision retried after the storage fault succeeds and lands
	// exactly once.
	retried, err := repo.SaveRepurposePlanRevision(ctx, plan, "recovered")
	if err != nil || retried.Revision != 2 {
		t.Fatalf("retry=%+v err=%v, want revision 2 to commit", retried, err)
	}
}

func TestConcurrentPlanApprovalAndRevisionSerialize(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "repurpose-concurrent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		plan, err := repo.SaveRepurposePlan(ctx, domain.RepurposePlan{Title: "first", Brief: "clip", DurationMS: 1000})
		if err != nil {
			t.Fatal(err)
		}
		first, err := repo.SaveRepurposePlanRevision(ctx, plan, "first")
		if err != nil {
			t.Fatal(err)
		}
		plan.Title = "second"
		start := make(chan struct{})
		approvals := make(chan error, 1)
		revisions := make(chan error, 1)
		go func() {
			<-start
			_, err := repo.ApproveRepurposePlanRevision(ctx, plan.ID, first.Revision)
			approvals <- err
		}()
		go func() {
			<-start
			_, err := repo.SaveRepurposePlanRevision(ctx, plan, "second")
			revisions <- err
		}()
		close(start)
		approvalErr, revisionErr := <-approvals, <-revisions
		wantTitle, wantState := "first", "approved"
		if approvalErr == nil {
			if !errors.Is(revisionErr, domain.ErrPlanImmutable) {
				t.Fatalf("revision after approval = %v", revisionErr)
			}
		} else {
			if !errors.Is(approvalErr, domain.ErrPlanRevisionNotLatest) || revisionErr != nil {
				t.Fatalf("approval = %v, revision = %v", approvalErr, revisionErr)
			}
			wantTitle, wantState = "second", "draft"
		}
		current, err := repo.GetRepurposePlan(ctx, plan.ID)
		if err != nil || current == nil || current.Title != wantTitle || current.Status != wantState {
			t.Fatalf("persisted plan = %+v, %v, want %q in state %q", current, err, wantTitle, wantState)
		}
	}
}

// TestApprovedPlanKeepsItsSnapshotAgainstRefusedWrites pins the acceptance
// shape for overlapping approve/revise operations: once approval has landed,
// refused saves and revisions leave the approved plan and its revision
// snapshot byte-for-byte intact — the operator's approved selection is the
// correct snapshot, not a half-applied draft.
func TestApprovedPlanKeepsItsSnapshotAgainstRefusedWrites(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "repurpose-approved-snapshot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	plan, err := repo.SaveRepurposePlan(ctx, domain.RepurposePlan{Brief: "approved plan", DurationMS: 30000, Title: "snapshot", Provider: "deterministic", Model: "heuristic-v1", Sections: []domain.PlanSection{{Role: "opening", Query: "city", DurationMS: 5000, Required: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SaveRepurposePlanRevision(ctx, plan, "initial"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ApproveRepurposePlanRevision(ctx, plan.ID, 1); err != nil {
		t.Fatal(err)
	}
	var approvedPlanJSON, approvedSnapshotJSON string
	if err := repo.db.QueryRowContext(ctx, `SELECT plan_json FROM repurpose_plans WHERE id=?`, plan.ID).Scan(&approvedPlanJSON); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT snapshot_json FROM repurpose_plan_revisions WHERE plan_id=? AND revision=1`, plan.ID).Scan(&approvedSnapshotJSON); err != nil {
		t.Fatal(err)
	}

	revised := plan
	revised.Brief = "half-applied draft"
	revised.Status = "draft"
	if _, err := repo.SaveRepurposePlanRevision(ctx, revised, "must not land"); !errors.Is(err, domain.ErrPlanImmutable) {
		t.Fatalf("revise after approval: want errors.Is(err, domain.ErrPlanImmutable); got %v", err)
	}
	if _, err := repo.SaveRepurposePlan(ctx, revised); !errors.Is(err, domain.ErrPlanImmutable) {
		t.Fatalf("save after approval: want errors.Is(err, domain.ErrPlanImmutable); got %v", err)
	}

	var planJSON, snapshotJSON string
	if err := repo.db.QueryRowContext(ctx, `SELECT plan_json FROM repurpose_plans WHERE id=?`, plan.ID).Scan(&planJSON); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT snapshot_json FROM repurpose_plan_revisions WHERE plan_id=? AND revision=1`, plan.ID).Scan(&snapshotJSON); err != nil {
		t.Fatal(err)
	}
	if planJSON != approvedPlanJSON || snapshotJSON != approvedSnapshotJSON {
		t.Fatalf("refused writes mutated approved state: plan=%s snapshot=%s", planJSON, snapshotJSON)
	}
}
