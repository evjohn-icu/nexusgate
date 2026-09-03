package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/domain"
	"github.com/evjohn-icu/nexusslate/internal/ingest"
)

// scanGateRepo implements the full Repository by embedding it as a nil field
// (the package pattern for fakes: unimplemented methods panic if called) and
// recording the scan-health calls ScanLibraryRoot makes. The recorded calls
// are the whole point: the gate's contract is which of them happen for which
// root state.
type scanGateRepo struct {
	Repository
	root      domain.LibraryRoot
	upsertErr error
	// markMissing is the count MarkUnseenLocationsMissing reports.
	markMissing int

	upserted []string // relative paths the scanner persisted

	scanStarted     bool
	healthy         bool
	unavailable     bool
	reconcileCalled bool
	reconcileSeen   []string
}

func (r *scanGateRepo) GetLibraryRoot(_ context.Context, _ string) (domain.LibraryRoot, error) {
	return r.root, nil
}

func (r *scanGateRepo) MarkRootScanStarted(_ context.Context, _ string, _ time.Time) error {
	r.scanStarted = true
	return nil
}

func (r *scanGateRepo) MarkRootHealthy(_ context.Context, _ string, _ time.Time) error {
	r.healthy = true
	return nil
}

func (r *scanGateRepo) MarkRootUnavailable(_ context.Context, _ string, _ time.Time) error {
	r.unavailable = true
	return nil
}

func (r *scanGateRepo) MarkUnseenLocationsMissing(_ context.Context, _ string, seen []string) (int, error) {
	r.reconcileCalled = true
	r.reconcileSeen = append([]string(nil), seen...)
	return r.markMissing, nil
}

func (r *scanGateRepo) UpsertScannedFile(_ context.Context, _ domain.LibraryRoot, relativePath, _ string, _ fs.FileInfo, _ string) (domain.ScannedFile, error) {
	r.upserted = append(r.upserted, relativePath)
	if r.upsertErr != nil {
		return domain.ScannedFile{}, r.upsertErr
	}
	return domain.ScannedFile{AssetID: "asset-" + relativePath, Created: true}, nil
}

func (r *scanGateRepo) AssetsWithoutProbeJob(context.Context, string, int) ([]string, error) {
	return nil, nil
}

// newScanGateService wires a Service around a scanGateRepo with the pieces
// ScanLibraryRoot needs: a scanner over the same fake, and an initialized
// scanFailures map (the enqueue tail of ScanLibraryRoot assigns into it).
func newScanGateService(repo *scanGateRepo) *Service {
	return &Service{
		repo:         repo,
		scanner:      ingest.NewScanner(repo),
		scanFailures: make(map[string]map[string]int),
	}
}

// An unavailable root must never reconcile: the scanner's walk of a missing
// path (or of an empty directory, which an unmounted NAS share is
// indistinguishable from) must not mark every previously-seen file missing.
// The gate's answer is MarkRootUnavailable, no MarkUnseenLocationsMissing,
// and a zero Missing count in the result.
func TestScanOfflineRootNeverReconciles(t *testing.T) {
	emptyRoot := t.TempDir() // present but empty: the unmounted-share shape
	missingRoot := filepath.Join(t.TempDir(), "never-mounted")

	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "root_directory_missing", path: missingRoot},
		{name: "root_directory_empty_looks_unmounted", path: emptyRoot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &scanGateRepo{root: domain.LibraryRoot{ID: "root-offline", Path: tc.path}}
			service := newScanGateService(repo)

			result, err := service.ScanLibraryRoot(context.Background(), "root-offline")
			if err != nil {
				t.Fatalf("scan of an offline root must not fail the caller: %v", err)
			}
			if !repo.scanStarted {
				t.Error("MarkRootScanStarted was not called")
			}
			if repo.reconcileCalled {
				t.Errorf("MarkUnseenLocationsMissing was called for an offline root with seen=%v", repo.reconcileSeen)
			}
			if !repo.unavailable {
				t.Error("MarkRootUnavailable was not called")
			}
			if repo.healthy {
				t.Error("MarkRootHealthy was called for an offline root")
			}
			if result.Missing != 0 {
				t.Errorf("Missing = %d, want 0: an offline root must not mark files missing", result.Missing)
			}
		})
	}
}

// A reachable, populated root reconciles: the walk's seen list is handed to
// MarkUnseenLocationsMissing, the root is marked healthy, and the returned
// count becomes the result's Missing.
func TestScanHealthyRootReconciles(t *testing.T) {
	rootDir := t.TempDir()
	scanWriteVideoFile(t, filepath.Join(rootDir, "clip.mp4"))
	if err := os.MkdirAll(filepath.Join(rootDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	scanWriteVideoFile(t, filepath.Join(rootDir, "sub", "later.mov"))

	repo := &scanGateRepo{root: domain.LibraryRoot{ID: "root-online", Path: rootDir}, markMissing: 3}
	service := newScanGateService(repo)

	result, err := service.ScanLibraryRoot(context.Background(), "root-online")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !repo.reconcileCalled {
		t.Fatal("MarkUnseenLocationsMissing was not called for a healthy root")
	}
	if len(repo.reconcileSeen) != 2 || repo.reconcileSeen[0] != "clip.mp4" || repo.reconcileSeen[1] != "sub/later.mov" {
		t.Errorf("reconcileSeen = %v, want [clip.mp4 sub/later.mov]", repo.reconcileSeen)
	}
	if !repo.healthy {
		t.Error("MarkRootHealthy was not called")
	}
	if repo.unavailable {
		t.Error("MarkRootUnavailable was called for a healthy root")
	}
	// Missing comes from the repository's return, as it did when the scanner
	// owned the call.
	if result.Missing != repo.markMissing {
		t.Errorf("Missing = %d, want %d (MarkUnseenLocationsMissing's return)", result.Missing, repo.markMissing)
	}
}

func TestIncompleteScanNeverReconcilesEvenWhenRootWasHealthy(t *testing.T) {
	rootDir := t.TempDir()
	scanWriteVideoFile(t, filepath.Join(rootDir, "clip.mp4"))
	repo := &scanGateRepo{root: domain.LibraryRoot{ID: "root-incomplete", Path: rootDir, HealthState: domain.RootHealthHealthy}}
	repo.upsertErr = errors.New("persist failure")
	service := newScanGateService(repo)
	// The scanner's fake persist failure makes the walk reachable but incomplete.
	result, err := service.ScanLibraryRoot(context.Background(), "root-incomplete")
	if err != nil {
		t.Fatal(err)
	}
	if result.Complete || repo.reconcileCalled || !repo.unavailable || repo.healthy {
		t.Fatalf("incomplete scan verdict: complete=%v reconcile=%v unavailable=%v healthy=%v", result.Complete, repo.reconcileCalled, repo.unavailable, repo.healthy)
	}
}

func TestRootHealthyAfterScanDoesNotTrustPriorHealth(t *testing.T) {
	service := &Service{}
	root := domain.LibraryRoot{Path: t.TempDir(), HealthState: domain.RootHealthHealthy}
	if service.rootHealthyAfterScan(root, domain.ScanResult{Complete: false, RootReachable: true}) {
		t.Fatal("prior healthy state authorized an incomplete scan")
	}
}
