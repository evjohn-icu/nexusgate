package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

func newRootHealthRepo(t *testing.T) (*Repository, domain.LibraryRoot) {
	t.Helper()
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "root-health.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateLibraryRoot(ctx, "/footage")
	if err != nil {
		t.Fatal(err)
	}
	return repo, root
}

// A freshly created root must read back as unknown with no health timestamps:
// reconciliation must not proceed on a root that was never verified, which is
// also the guarantee existing pre-migration rows get from the column default.
func TestLibraryRootHealthDefaults(t *testing.T) {
	ctx := context.Background()
	repo, root := newRootHealthRepo(t)
	got, err := repo.GetLibraryRoot(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HealthState != domain.RootHealthUnknown {
		t.Fatalf("HealthState = %q, want %q", got.HealthState, domain.RootHealthUnknown)
	}
	if got.LastHealthyAt != nil {
		t.Fatalf("LastHealthyAt = %v, want nil", got.LastHealthyAt)
	}
	if got.LastScanAt != nil {
		t.Fatalf("LastScanAt = %v, want nil", got.LastScanAt)
	}
}

func TestMarkRootHealthy(t *testing.T) {
	ctx := context.Background()
	repo, root := newRootHealthRepo(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	if err := repo.MarkRootHealthy(ctx, root.ID, at); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetLibraryRoot(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HealthState != domain.RootHealthHealthy {
		t.Fatalf("HealthState = %q, want %q", got.HealthState, domain.RootHealthHealthy)
	}
	if got.LastHealthyAt == nil || !got.LastHealthyAt.Equal(at) {
		t.Fatalf("LastHealthyAt = %v, want %v", got.LastHealthyAt, at)
	}
	if got.LastScanAt == nil || !got.LastScanAt.Equal(at) {
		t.Fatalf("LastScanAt = %v, want %v", got.LastScanAt, at)
	}
}

// An unavailable scan must not erase the last verified health timestamp: the
// UI's "last healthy" display needs it while the root is down.
func TestMarkRootUnavailablePreservesLastHealthy(t *testing.T) {
	ctx := context.Background()
	repo, root := newRootHealthRepo(t)
	healthyAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	if err := repo.MarkRootHealthy(ctx, root.ID, healthyAt); err != nil {
		t.Fatal(err)
	}
	downAt := time.Now().UTC().Truncate(time.Millisecond)
	if err := repo.MarkRootUnavailable(ctx, root.ID, downAt); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetLibraryRoot(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HealthState != domain.RootHealthUnavailable {
		t.Fatalf("HealthState = %q, want %q", got.HealthState, domain.RootHealthUnavailable)
	}
	if got.LastHealthyAt == nil || !got.LastHealthyAt.Equal(healthyAt) {
		t.Fatalf("LastHealthyAt = %v, want preserved %v", got.LastHealthyAt, healthyAt)
	}
	if got.LastScanAt == nil || !got.LastScanAt.Equal(downAt) {
		t.Fatalf("LastScanAt = %v, want %v", got.LastScanAt, downAt)
	}
}

func TestMarkRootScanStarted(t *testing.T) {
	ctx := context.Background()
	repo, root := newRootHealthRepo(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	if err := repo.MarkRootScanStarted(ctx, root.ID, at); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetLibraryRoot(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HealthState != domain.RootHealthUnknown {
		t.Fatalf("HealthState = %q, want %q (a scan start is not a health verdict)", got.HealthState, domain.RootHealthUnknown)
	}
	if got.LastScanAt == nil || !got.LastScanAt.Equal(at) {
		t.Fatalf("LastScanAt = %v, want %v", got.LastScanAt, at)
	}
	if got.LastHealthyAt != nil {
		t.Fatalf("LastHealthyAt = %v, want nil", got.LastHealthyAt)
	}
}

// ListLibraryRoots and GetLibraryRoot must agree on the health fields, since
// the UI renders the list from ListLibraryRoots.
func TestLibraryRootHealthRoundTripsThroughList(t *testing.T) {
	ctx := context.Background()
	repo, root := newRootHealthRepo(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	if err := repo.MarkRootHealthy(ctx, root.ID, at); err != nil {
		t.Fatal(err)
	}
	roots, err := repo.ListLibraryRoots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 {
		t.Fatalf("ListLibraryRoots returned %d roots, want 1", len(roots))
	}
	got := roots[0]
	if got.HealthState != domain.RootHealthHealthy {
		t.Fatalf("HealthState = %q, want %q", got.HealthState, domain.RootHealthHealthy)
	}
	if got.LastHealthyAt == nil || !got.LastHealthyAt.Equal(at) {
		t.Fatalf("LastHealthyAt = %v, want %v", got.LastHealthyAt, at)
	}
	if got.LastScanAt == nil || !got.LastScanAt.Equal(at) {
		t.Fatalf("LastScanAt = %v, want %v", got.LastScanAt, at)
	}
}
