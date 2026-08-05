package app

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// approveRaceRepo wraps a real *sqlite.Repository and, on the first call to
// ApproveRepurposePlanRevision, lets a second "caller" (winner) run its own
// full approve to completion first. This deterministically constructs the
// TOCTOU window ApproveRepurposePlanRevision's doc comment describes -- two
// callers approving the same revision, where the second one already read the
// revision as an unapproved draft before the first one committed -- instead
// of hoping raw goroutine scheduling happens to interleave that way.
// ListRepurposePlanRevisions is untouched, so it always sees whatever is
// actually committed at the time it runs; only the write itself is delayed
// behind the winner.
type approveRaceRepo struct {
	*sqlite.Repository
	once   sync.Once
	winner func()
}

func (r *approveRaceRepo) ApproveRepurposePlanRevision(ctx context.Context, planID string, revision int) (domain.RepurposePlanRevision, error) {
	r.once.Do(func() {
		if r.winner != nil {
			r.winner()
		}
	})
	return r.Repository.ApproveRepurposePlanRevision(ctx, planID, revision)
}

// TestApproveRepurposePlanRevisionClassifiesWriteTimeRecheck drives the
// repository's in-transaction check in ApproveRepurposePlanRevision
// (internal/repository/sqlite/repository.go) and asserts the failure reaches
// the caller as app.ErrPlanRevisionNotDraft.
//
// The two calls are sequenced, not launched as racing goroutines: the losing
// write is only reachable by winning a real race against SQLite's own locking
// a fraction of the time, which makes a goroutine-based version of this test
// flaky by construction. approveRaceRepo forces the exact interleaving a
// genuine race would occasionally produce -- the loser's section-selection
// read sees revision 1 as an unapproved draft, the winner commits its
// approval in between, and the loser's write then lands against
// already-approved state -- every time, deterministically.
//
// This is the shape no check outside the transaction can catch, which is why
// the repository's is the only one: Service asks nothing about "draft" or
// "latest" before calling, so the sentinel this asserts on can only have come
// from the write itself.
func TestApproveRepurposePlanRevisionClassifiesWriteTimeRecheck(t *testing.T) {
	ctx := context.Background()
	real, err := sqlite.Open(filepath.Join(t.TempDir(), "approve-race.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer real.Close()
	if err := real.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	plan, err := real.SaveRepurposePlan(ctx, domain.RepurposePlan{
		Brief: "race test", DurationMS: 10000, Title: "race", Status: "draft",
		Provider: "deterministic", Model: "heuristic-v1",
		Sections: []domain.PlanSection{{Role: "opening", Query: "city", DurationMS: 5000, Required: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := real.SaveRepurposePlanRevision(ctx, plan, "initial"); err != nil {
		t.Fatal(err)
	}

	race := &approveRaceRepo{Repository: real, winner: func() {
		// The concurrent winner: approves revision 1 directly against the
		// real repository (bypassing the wrapper, so this call is not
		// itself delayed) before the loser's write below is allowed to run.
		if _, err := real.ApproveRepurposePlanRevision(ctx, plan.ID, 1); err != nil {
			t.Fatalf("winner approve: %v", err)
		}
	}}
	svc := &Service{repo: race}

	// The loser: Service.ApproveRepurposePlanRevision's section-selection
	// read (ListRepurposePlanRevisions) runs first and still sees revision 1
	// as an unapproved draft -- the winner has not run yet -- so this call
	// reaches s.repo.ApproveRepurposePlanRevision, which is where
	// approveRaceRepo lets the winner in.
	_, err = svc.ApproveRepurposePlanRevision(ctx, plan.ID, 1)
	if err == nil {
		t.Fatal("expected the loser's approve to fail once the winner already approved the same revision")
	}
	if !errors.Is(err, ErrPlanRevisionNotDraft) {
		t.Fatalf("expected errors.Is(err, ErrPlanRevisionNotDraft); got %v", err)
	}
	// Not-latest must NOT be the one that fired here: revision 1 is still
	// the plan's latest, so a not-latest answer would mean the classification
	// came from somewhere other than the state the write actually saw.
	if errors.Is(err, ErrPlanRevisionNotLatest) {
		t.Fatalf("classified as not-latest instead of not-draft: %v", err)
	}
}

// reviseRaceRepo is approveRaceRepo's counterpart for the other TOCTOU shape
// this boundary names: one caller revising a plan while another approves it.
// SaveRepurposePlanRevision is where the immutability check that decides runs,
// so delaying the loser there behind a winning ApproveRepurposePlanRevision
// forces that check -- not ReviseRepurposePlan's plan.Status=="approved"
// ordering guard, which by then has already been passed -- to be what fires.
type reviseRaceRepo struct {
	*sqlite.Repository
	once   sync.Once
	winner func()
}

func (r *reviseRaceRepo) SaveRepurposePlanRevision(ctx context.Context, plan domain.RepurposePlan, editorNote string) (domain.RepurposePlanRevision, error) {
	r.once.Do(func() {
		if r.winner != nil {
			r.winner()
		}
	})
	return r.Repository.SaveRepurposePlanRevision(ctx, plan, editorNote)
}

// TestReviseRepurposePlanClassifiesWriteTimeRecheck is the "one caller
// revises while another approves" shape of the same TOCTOU window: the
// reviser's plan==nil/approved ordering guard in ReviseRepurposePlan reads
// the plan as still draft, then loses the race to an approval that lands
// before the reviser's write reaches the check inside
// SaveRepurposePlanRevision that decides.
func TestReviseRepurposePlanClassifiesWriteTimeRecheck(t *testing.T) {
	ctx := context.Background()
	real, err := sqlite.Open(filepath.Join(t.TempDir(), "revise-race.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer real.Close()
	if err := real.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	plan, err := real.SaveRepurposePlan(ctx, domain.RepurposePlan{
		Brief: "revise race", DurationMS: 10000, Title: "race", Status: "draft",
		Provider: "deterministic", Model: "heuristic-v1",
		Sections: []domain.PlanSection{{Role: "opening", Query: "city", DurationMS: 5000, Required: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := real.SaveRepurposePlanRevision(ctx, plan, "initial"); err != nil {
		t.Fatal(err)
	}

	race := &reviseRaceRepo{Repository: real, winner: func() {
		// The concurrent winner: approves revision 1 directly against the
		// real repository before the loser's revise write below runs.
		if _, err := real.ApproveRepurposePlanRevision(ctx, plan.ID, 1); err != nil {
			t.Fatalf("winner approve: %v", err)
		}
	}}
	svc := &Service{repo: race}

	// The loser: ReviseRepurposePlan's ordering guard (GetRepurposePlan) runs
	// first and still sees the plan as draft -- the winner has not approved
	// yet -- so this call passes the guard and reaches
	// s.repo.SaveRepurposePlanRevision, which is where reviseRaceRepo lets
	// the winner in. No candidates on the section, so the required-section
	// check only adds to MissingNeeds rather than needing ShotExists.
	sections := []domain.PlanSection{{Role: "opening", Query: "city", DurationMS: 5000, Required: true}}
	_, err = svc.ReviseRepurposePlan(ctx, plan.ID, sections, "editor tweak")
	if err == nil {
		t.Fatal("expected the loser's revise to fail once the winner already approved the plan")
	}
	if !errors.Is(err, ErrPlanImmutable) {
		t.Fatalf("expected errors.Is(err, ErrPlanImmutable); got %v", err)
	}
	if errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("classified as not-found instead of immutable: %v", err)
	}
}
