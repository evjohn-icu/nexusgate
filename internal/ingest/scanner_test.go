package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// stubScanRepo is an in-memory stub implementing ScanRepository. It has no
// mark-missing method: the scanner deliberately does not reconcile — it
// returns the seen list and the app service decides whether that list may be
// used to mark files missing.
type stubScanRepo struct {
	// per-file callback: if set, invoked for each UpsertScannedFile call.
	upsertFn func(ctx context.Context, root domain.LibraryRoot, relativePath, absolutePath string, info fs.FileInfo, fingerprint string) (domain.ScannedFile, error)
	// record of calls for assertions.
	upsertCalls []string // relative paths seen
	// known is the fingerprint cache the scanner consults before reading a
	// file, keyed by relative path. Empty by default, so every existing test
	// in this file still exercises the full read.
	known map[string]domain.KnownFile
	// knownCalls counts cache lookups so a test can tell "the cache said no"
	// from "the cache was never asked".
	knownCalls   int
	knownErr     error
	fingerprints []string // the fingerprint handed to each UpsertScannedFile call
}

func (s *stubScanRepo) KnownFile(ctx context.Context, rootID, relativePath string) (domain.KnownFile, bool, error) {
	s.knownCalls++
	if s.knownErr != nil {
		return domain.KnownFile{}, false, s.knownErr
	}
	known, ok := s.known[relativePath]
	return known, ok, nil
}

func (s *stubScanRepo) UpsertScannedFile(ctx context.Context, root domain.LibraryRoot, relativePath, absolutePath string, info fs.FileInfo, fingerprint string) (domain.ScannedFile, error) {
	s.upsertCalls = append(s.upsertCalls, relativePath)
	s.fingerprints = append(s.fingerprints, fingerprint)
	if s.upsertFn != nil {
		return s.upsertFn(ctx, root, relativePath, absolutePath, info, fingerprint)
	}
	// default: discovered, not changed
	return domain.ScannedFile{AssetID: "asset-" + relativePath, Created: true, Changed: false}, nil
}

// helpers

// writeFile creates a file with minimal video-looking content (enough for
// QuickFingerprint to compute a fingerprint).
func writeFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	// Write enough bytes so QuickFingerprint can read sample offsets (4 MiB
	// sample size, so a 5-byte file exercises EOF-truncated reads).
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeDir creates an empty directory.
func writeDir(t *testing.T, parent, name string) string {
	t.Helper()
	p := filepath.Join(parent, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// --------------- supportedVideo table-driven tests ---------------

func TestSupportedVideo(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		// recognised extensions (lowercase)
		{"clip.mov", true},
		{"clip.mp4", true},
		{"clip.m4v", true},
		{"clip.mxf", true},
		{"clip.braw", true},
		{"clip.r3d", true},
		{"clip.ari", true},
		{"clip.crm", true},
		{"clip.dng", true},
		{"clip.nev", true},
		{"clip.insv", true},
		// recognised extensions (uppercase / mixed)
		{"CLIP.MOV", true},
		{"CLIP.MP4", true},
		{"Clip.Braw", true},
		{"Clip.R3D", true},
		// recognised extensions with leading dot in filename
		{".hidden.mov", true},
		{".hidden.mp4", true},
		// unknown / unsupported extensions
		{"image.jpg", false},
		{"image.png", false},
		{"notes.txt", false},
		{"sidecar.srt", false},
		{"sidecar.xmp", false},
		{"audio.wav", false},
		{"audio.mp3", false},
		// empty string
		{"", false},
		// no extension
		{"noext", false},
		{"Makefile", false},
		// leading dot only
		{".", false},
		{"/", false},
		// path with dots in dir names but no video ext
		{"DCIM/100MEDIA/IMG_0001.jpg", false},
		// path with video ext in subdir
		{"DCIM/100MEDIA/CLIP0001.mp4", true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			got := supportedVideo(tc.path)
			if got != tc.want {
				t.Errorf("supportedVideo(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestSupportedVideoRecognizesMainstreamCameraMedia(t *testing.T) {
	for _, path := range []string{
		"A001_08151512_C001.braw", // Blackmagic RAW
		"A001_C001_0101AB.r3d",    // REDCODE RAW
		"A001C001_240101.ari",     // ARRIRAW
		"C0001.crm",               // Canon Cinema RAW Light
		"frame.dng",               // CinemaDNG still frame
		"NRAW_0001.nev",           // Nikon N-RAW
		"DJI_0001.mp4",
		"C0001.mxf",             // Sony / Canon / Panasonic professional media
		"VID_20260726_001.insv", // Insta360 source
	} {
		if !supportedVideo(path) {
			t.Errorf("supportedVideo(%q) = false, want true", path)
		}
	}
}

func TestSupportedVideoRejectsSidecarsAndStillImages(t *testing.T) {
	for _, path := range []string{"DJI_0001.srt", "A001.sidecar", "IMG_0001.dng.xmp", "frame.jpg"} {
		if supportedVideo(path) {
			t.Errorf("supportedVideo(%q) = true, want false", path)
		}
	}
}

// --------------- Scan success path tests ---------------

func TestScanSingleVideoFile(t *testing.T) {
	root := writeDir(t, t.TempDir(), "root")
	writeFile(t, root, "clip.mov")

	repo := &stubScanRepo{}
	s := NewScanner(repo)

	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Discovered != 1 {
		t.Errorf("Discovered = %d, want 1", result.Discovered)
	}
	if result.Linked != 0 {
		t.Errorf("Linked = %d, want 0", result.Linked)
	}
	if len(repo.upsertCalls) != 1 || repo.upsertCalls[0] != "clip.mov" {
		t.Errorf("upsertCalls = %v, want [clip.mov]", repo.upsertCalls)
	}
	// The seen list is handed back, not applied: reconciliation is decided by
	// the service's root-health gate, so the scanner only reports.
	if len(result.SeenRelativePaths) != 1 || result.SeenRelativePaths[0] != "clip.mov" {
		t.Errorf("SeenRelativePaths = %v, want [clip.mov]", result.SeenRelativePaths)
	}
}

func TestScanNestedDirectories(t *testing.T) {
	root := writeDir(t, t.TempDir(), "root")
	writeDir(t, root, "DCIM")
	writeDir(t, root, "DCIM/100MEDIA")
	writeFile(t, root, "DCIM/100MEDIA/clip001.mp4")
	writeFile(t, root, "DCIM/100MEDIA/clip002.mxf")
	writeFile(t, root, "clip003.mov") // at root level

	repo := &stubScanRepo{}
	s := NewScanner(repo)

	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Discovered != 3 {
		t.Errorf("Discovered = %d, want 3", result.Discovered)
	}
	if len(repo.upsertCalls) != 3 {
		t.Fatalf("upsertCalls len = %d, want 3: %v", len(repo.upsertCalls), repo.upsertCalls)
	}
	// All relative paths should use slash separator.
	for _, p := range repo.upsertCalls {
		if strings.Contains(p, "\\") {
			t.Errorf("upsert path contains backslash: %q", p)
		}
	}
}

func TestScanEmptyDirectory(t *testing.T) {
	root := writeDir(t, t.TempDir(), "root")

	repo := &stubScanRepo{}
	s := NewScanner(repo)

	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Discovered != 0 {
		t.Errorf("Discovered = %d, want 0", result.Discovered)
	}
	if len(repo.upsertCalls) != 0 {
		t.Errorf("upsertCalls = %v, want empty", repo.upsertCalls)
	}
	// An empty walk returns an empty seen list WITHOUT reconciling: the
	// scanner must never mark files missing itself, because an unmounted NAS
	// root walks exactly like this. Whether the empty seen list is applied
	// (mark everything missing) is the service's root-health gate's call.
	if len(result.SeenRelativePaths) != 0 {
		t.Errorf("SeenRelativePaths = %v, want empty", result.SeenRelativePaths)
	}
	if result.Missing != 0 {
		t.Errorf("Missing = %d, want 0: the scanner does not reconcile", result.Missing)
	}
}

func TestScanFiltersNonVideoFiles(t *testing.T) {
	root := writeDir(t, t.TempDir(), "root")
	writeFile(t, root, "clip.mp4")
	writeFile(t, root, "notes.txt")
	writeFile(t, root, "poster.jpg")
	writeFile(t, root, "audio.wav")
	writeFile(t, root, "sidecar.srt")
	writeFile(t, root, "sidecar.xmp")
	writeFile(t, root, "Makefile")
	writeFile(t, root, "README.md")
	writeFile(t, root, "clip2.mov")

	repo := &stubScanRepo{}
	s := NewScanner(repo)

	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Discovered != 2 {
		t.Errorf("Discovered = %d, want 2 (only .mp4 and .mov)", result.Discovered)
	}
	if len(repo.upsertCalls) != 2 {
		t.Errorf("upsertCalls len = %d, want 2: %v", len(repo.upsertCalls), repo.upsertCalls)
	}
	for _, p := range repo.upsertCalls {
		if filepath.Ext(p) == ".txt" || filepath.Ext(p) == ".jpg" {
			t.Errorf("non-video file %q was processed", p)
		}
	}
}

func TestScanAllSupportedExtensions(t *testing.T) {
	root := writeDir(t, t.TempDir(), "root")
	exts := []string{".mov", ".mp4", ".m4v", ".mxf", ".braw", ".r3d", ".ari", ".crm", ".dng", ".nev", ".insv"}
	for _, ext := range exts {
		writeFile(t, root, "clip"+ext)
	}

	repo := &stubScanRepo{}
	s := NewScanner(repo)

	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Discovered != len(exts) {
		t.Errorf("Discovered = %d, want %d", result.Discovered, len(exts))
	}
}

func TestScanChangedAssetIDs(t *testing.T) {
	root := writeDir(t, t.TempDir(), "root")
	writeFile(t, root, "A.mp4")
	writeFile(t, root, "B.mov")
	writeFile(t, root, "C.mxf")

	call := 0
	repo := &stubScanRepo{
		upsertFn: func(ctx context.Context, root domain.LibraryRoot, relativePath, absolutePath string, info fs.FileInfo, fingerprint string) (domain.ScannedFile, error) {
			call++
			// A: created, not changed. B: linked (already existed), changed. C: created, changed.
			switch filepath.Base(relativePath) {
			case "A.mp4":
				return domain.ScannedFile{AssetID: "asset-A", Created: true, Changed: false}, nil
			case "B.mov":
				return domain.ScannedFile{AssetID: "asset-B", Created: false, Changed: true}, nil
			case "C.mxf":
				return domain.ScannedFile{AssetID: "asset-C", Created: true, Changed: true}, nil
			}
			return domain.ScannedFile{}, nil
		},
	}
	s := NewScanner(repo)

	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Discovered != 2 { // A and C created
		t.Errorf("Discovered = %d, want 2", result.Discovered)
	}
	if result.Linked != 1 { // B linked
		t.Errorf("Linked = %d, want 1", result.Linked)
	}
	// ChangedAssetIDs should contain B and C, not A.
	changed := result.ChangedAssetIDs
	if len(changed) != 2 {
		t.Fatalf("ChangedAssetIDs len = %d, want 2: %v", len(changed), changed)
	}
	want := map[string]bool{"asset-B": true, "asset-C": true}
	for _, id := range changed {
		if !want[id] {
			t.Errorf("unexpected ChangedAssetID %q", id)
		}
	}
}

func TestScanChangedAssetIDsDeduplication(t *testing.T) {
	// Verify deduplication: same asset appears twice (via different paths),
	// ChangedAssetIDs only records it once.
	root := writeDir(t, t.TempDir(), "root")
	writeFile(t, root, "A.mp4")
	writeFile(t, root, "B.mp4")

	repo := &stubScanRepo{
		upsertFn: func(ctx context.Context, root domain.LibraryRoot, relativePath, absolutePath string, info fs.FileInfo, fingerprint string) (domain.ScannedFile, error) {
			// Both files resolve to the same asset.
			return domain.ScannedFile{AssetID: "asset-shared", Created: false, Changed: true}, nil
		},
	}
	s := NewScanner(repo)

	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.ChangedAssetIDs) != 1 {
		t.Errorf("ChangedAssetIDs = %v, want [asset-shared] (deduplicated)", result.ChangedAssetIDs)
	}
}

// TestScannerNoLongerReconciles pins the boundary of the O1b change: the
// scanner returns the seen list and leaves the Missing count at zero — the
// service calls MarkUnseenLocationsMissing itself, and only after its
// root-health gate has passed. A scanner that marks files missing would turn
// an unmounted NAS root's empty walk into a mass missing-file verdict before
// the gate ever ran.
func TestScannerNoLongerReconciles(t *testing.T) {
	root := writeDir(t, t.TempDir(), "root")
	writeFile(t, root, "clip.mp4")

	repo := &stubScanRepo{}
	s := NewScanner(repo)

	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Missing != 0 {
		t.Errorf("Missing = %d, want 0 (reconciliation moved to the service)", result.Missing)
	}
	if len(result.SeenRelativePaths) != 1 || result.SeenRelativePaths[0] != "clip.mp4" {
		t.Errorf("SeenRelativePaths = %v, want [clip.mp4]", result.SeenRelativePaths)
	}
}

func TestScanErrorsCollected(t *testing.T) {
	// UpsertScannedFile returns an error for one file; walk continues and
	// collects it.
	root := writeDir(t, t.TempDir(), "root")
	writeFile(t, root, "good.mp4")
	writeFile(t, root, "bad.mov")

	repo := &stubScanRepo{
		upsertFn: func(ctx context.Context, root domain.LibraryRoot, relativePath, absolutePath string, info fs.FileInfo, fingerprint string) (domain.ScannedFile, error) {
			if filepath.Base(relativePath) == "bad.mov" {
				return domain.ScannedFile{}, errors.New("db timeout")
			}
			return domain.ScannedFile{AssetID: "asset-good", Created: true}, nil
		},
	}
	s := NewScanner(repo)

	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Discovered != 1 {
		t.Errorf("Discovered = %d, want 1 (good.mp4 only)", result.Discovered)
	}
	found := false
	for _, e := range result.Errors {
		if strings.Contains(e, "db timeout") {
			found = true
		}
	}
	if !found {
		t.Errorf("Errors does not contain 'db timeout': %v", result.Errors)
	}
}

// --------------- Scan error propagation tests ---------------

func TestScanRepoUpsertErrorDoesNotStopWalk(t *testing.T) {
	// Errors from UpsertScannedFile are collected, not propagated as return error.
	root := writeDir(t, t.TempDir(), "root")
	writeFile(t, root, "A.mp4")
	writeFile(t, root, "B.mov")

	repo := &stubScanRepo{
		upsertFn: func(ctx context.Context, root domain.LibraryRoot, relativePath, absolutePath string, info fs.FileInfo, fingerprint string) (domain.ScannedFile, error) {
			return domain.ScannedFile{}, errors.New("persist error")
		},
	}
	s := NewScanner(repo)

	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: root})
	if err != nil {
		t.Fatalf("upsert errors should not stop the walk: %v", err)
	}
	if len(result.Errors) != 2 {
		t.Errorf("Errors len = %d, want 2", len(result.Errors))
	}
	if len(result.SeenRelativePaths) != 0 {
		t.Errorf("SeenRelativePaths len = %d, want 0 for failed persists", len(result.SeenRelativePaths))
	}
	if result.Complete {
		t.Error("failed persists must make scan incomplete")
	}
}

func TestScanContextCancellation(t *testing.T) {
	root := writeDir(t, t.TempDir(), "root")
	writeFile(t, root, "A.mp4")
	writeFile(t, root, "B.mov")
	writeFile(t, root, "C.mxf")

	repo := &stubScanRepo{
		upsertFn: func(ctx context.Context, root domain.LibraryRoot, relativePath, absolutePath string, info fs.FileInfo, fingerprint string) (domain.ScannedFile, error) {
			// Cancel after first file is processed.
			return domain.ScannedFile{AssetID: "asset", Created: true}, nil
		},
	}
	s := NewScanner(repo)

	ctx, cancel := context.WithCancel(context.Background())
	// Instead of complex timing, pre-cancel the context so the walk callback
	// checks ctx.Err() on the very first file entry.
	cancel()

	_, err := s.Scan(ctx, domain.LibraryRoot{ID: "r1", Path: root})
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
}

func TestScanRootDoesNotExist(t *testing.T) {
	// filepath.WalkDir passes the stat error as walkErr to the callback,
	// which collects it into result.Errors and returns nil, so WalkDir
	// itself returns nil. The error is surfaced through result.Errors.
	repo := &stubScanRepo{}
	s := NewScanner(repo)

	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: "/nonexistent/path/for/test"})
	if err != nil {
		t.Fatalf("WalkDir returns nil even for nonexistent root: %v", err)
	}
	if len(result.Errors) == 0 {
		t.Fatal("expected walkErr in result.Errors, got none")
	}
	if result.Complete || result.RootReachable {
		t.Fatalf("missing root result = complete=%v reachable=%v, want false/false", result.Complete, result.RootReachable)
	}
	if !strings.HasPrefix(result.Errors[0], "walk root: ") {
		t.Fatalf("root error = %q, want stable walk root prefix", result.Errors[0])
	}
	// Nothing was seen and nothing was reconciled: whether an unreachable
	// root's empty walk may mark files missing is the service's gate to
	// decide (it must not — see root_offline_gate_test.go), not the
	// scanner's.
	if len(result.SeenRelativePaths) != 0 {
		t.Errorf("SeenRelativePaths = %v, want empty", result.SeenRelativePaths)
	}
	if result.Missing != 0 {
		t.Errorf("Missing = %d, want 0", result.Missing)
	}
}

// --------------- Regression: relative path normalization ---------------

func TestScanRelativePathUsesSlash(t *testing.T) {
	root := writeDir(t, t.TempDir(), "root")
	writeFile(t, root, "sub/dir/clip.mp4")

	repo := &stubScanRepo{}
	s := NewScanner(repo)

	_, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(repo.upsertCalls) != 1 {
		t.Fatalf("upsertCalls len = %d", len(repo.upsertCalls))
	}
	if repo.upsertCalls[0] != "sub/dir/clip.mp4" {
		t.Errorf("relative path = %q, want sub/dir/clip.mp4", repo.upsertCalls[0])
	}
}

// --------------- ChangedAssetIDs empty when no changes ---------------

func TestScanNoChangedAssets(t *testing.T) {
	root := writeDir(t, t.TempDir(), "root")
	writeFile(t, root, "A.mp4")

	repo := &stubScanRepo{
		upsertFn: func(ctx context.Context, root domain.LibraryRoot, relativePath, absolutePath string, info fs.FileInfo, fingerprint string) (domain.ScannedFile, error) {
			return domain.ScannedFile{AssetID: "asset-A", Created: true, Changed: false}, nil
		},
	}
	s := NewScanner(repo)

	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.ChangedAssetIDs) != 0 {
		t.Errorf("ChangedAssetIDs = %v, want empty", result.ChangedAssetIDs)
	}
}

// --------------- Edge: file with dots in name ---------------

func TestScanFileWithMultipleDots(t *testing.T) {
	root := writeDir(t, t.TempDir(), "root")
	writeFile(t, root, "A001_08151512_C001.braw")
	writeFile(t, root, "project.v1.final.mp4")
	writeFile(t, root, "archive.tar.gz") // not video

	repo := &stubScanRepo{}
	s := NewScanner(repo)

	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Discovered != 2 {
		t.Errorf("Discovered = %d, want 2", result.Discovered)
	}
	if len(repo.upsertCalls) != 2 {
		t.Errorf("upsertCalls = %v, want 2 entries", repo.upsertCalls)
	}
}

// --------------- Edge: non-media files when repo returns changed ---------------

func TestScanNonMediaNotProcessed(t *testing.T) {
	root := writeDir(t, t.TempDir(), "root")
	writeFile(t, root, "notes.txt")
	writeFile(t, root, "image.jpg")

	repo := &stubScanRepo{}
	s := NewScanner(repo)

	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Discovered != 0 {
		t.Errorf("Discovered = %d, want 0 (no media files)", result.Discovered)
	}
	if len(repo.upsertCalls) != 0 {
		t.Errorf("non-media files should not be upserted: %v", repo.upsertCalls)
	}
}

// TestScanSubdirectoryIsSkipped verifies that subdirectories themselves are
// not passed to the walk callback for Upsert — only files inside them.
// filepath.WalkDir only calls the callback for directories, but we return nil
// (no error) for them.
func TestScanSubdirectoryIsSkippedWithoutError(t *testing.T) {
	root := writeDir(t, t.TempDir(), "root")
	writeDir(t, root, "subdir")
	writeFile(t, root, "subdir/clip.mp4")
	writeFile(t, root, "rootclip.mov")

	repo := &stubScanRepo{}
	s := NewScanner(repo)

	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Discovered != 2 {
		t.Errorf("Discovered = %d, want 2", result.Discovered)
	}
}

// --------------- Deep nesting ---------------

func TestScanDeeplyNestedDirectories(t *testing.T) {
	root := writeDir(t, t.TempDir(), "root")
	writeFile(t, root, "a/b/c/d/e/clip.mp4")
	writeFile(t, root, "a/b/c/clip2.mov")

	repo := &stubScanRepo{}
	s := NewScanner(repo)

	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Discovered != 2 {
		t.Errorf("Discovered = %d, want 2", result.Discovered)
	}
}

// --------------- Saw zero Upsert calls when root has only dirs ---------------

func TestScanOnlyDirectoriesNoFiles(t *testing.T) {
	root := writeDir(t, t.TempDir(), "root")
	writeDir(t, root, "empty")
	writeDir(t, root, "also_empty/nested")

	repo := &stubScanRepo{}
	s := NewScanner(repo)

	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Discovered != 0 {
		t.Errorf("Discovered = %d, want 0", result.Discovered)
	}
	if len(result.Errors) != 0 {
		t.Errorf("Errors = %v, want empty", result.Errors)
	}
}

// --------------- QuickFingerprint error propagation (file too small, etc.) ---------------
// QuickFingerprint works fine with small files (it handles EOF-truncated reads),
// but a genuinely unreadable file is tested below.

// permissionsAreEnforced reports whether this process is genuinely refused a
// file it holds no read bit for. It is a behavioural probe rather than an
// os.Getuid() == 0 check because those are different questions:
// CAP_DAC_OVERRIDE can sit in an ordinary process's *ambient* capability set
// — a common container default, and what this repository's dev shell hands to
// uid 1000 — and a process holding it opens a mode-000 file exactly as root
// would. A test that builds an unreadable file in order to assert a refusal
// has nothing left to assert there, and asserting it anyway makes the suite
// permanently red for an environment reason with no bearing on the code.
func permissionsAreEnforced(t *testing.T) bool {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "permission-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(probe, 0o644) })
	f, err := os.Open(probe)
	if err != nil {
		return true
	}
	_ = f.Close()
	return false
}

func TestScanUnreadableFileSkipsWithError(t *testing.T) {
	if !permissionsAreEnforced(t) {
		t.Skip("this process can read a mode-000 file (root, or CAP_DAC_OVERRIDE in the ambient set), so an unreadable file cannot be built here")
	}
	root := writeDir(t, t.TempDir(), "root")
	badFile := filepath.Join(root, "bad.mov")
	// Create a file with no read permission so QuickFingerprint fails.
	if err := os.WriteFile(badFile, []byte("secret"), 0o000); err != nil {
		t.Fatal(err)
	}
	// Restore permission so TempDir cleanup can remove it.
	t.Cleanup(func() { os.Chmod(badFile, 0o644) })

	repo := &stubScanRepo{}
	s := NewScanner(repo)

	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// bad.mov passes supportedVideo and !IsDir, but QuickFingerprint fails
	// with a permission error during os.Open.
	if len(result.Errors) == 0 {
		t.Error("expected fingerprint error for unreadable file, got none")
	}
}

// --------------- ChangedAssetIDs with mixed changed/unchanged ---------------

func TestScanMixedChangedAndUnchangedAssets(t *testing.T) {
	root := writeDir(t, t.TempDir(), "root")
	writeFile(t, root, "unchanged.mp4")
	writeFile(t, root, "changed.mov")
	writeFile(t, root, "also_changed.mxf")

	repo := &stubScanRepo{
		upsertFn: func(ctx context.Context, root domain.LibraryRoot, relativePath, absolutePath string, info fs.FileInfo, fingerprint string) (domain.ScannedFile, error) {
			base := filepath.Base(relativePath)
			switch base {
			case "unchanged.mp4":
				return domain.ScannedFile{AssetID: "asset-unchanged", Created: false, Changed: false}, nil
			case "changed.mov":
				return domain.ScannedFile{AssetID: "asset-changed", Created: false, Changed: true}, nil
			case "also_changed.mxf":
				return domain.ScannedFile{AssetID: "asset-also", Created: true, Changed: true}, nil
			}
			return domain.ScannedFile{}, nil
		},
	}
	s := NewScanner(repo)

	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Discovered != 1 { // only also_changed is created
		t.Errorf("Discovered = %d, want 1", result.Discovered)
	}
	if result.Linked != 2 { // unchanged + changed already existed
		t.Errorf("Linked = %d, want 2", result.Linked)
	}
	if len(result.ChangedAssetIDs) != 2 {
		t.Errorf("ChangedAssetIDs len = %d, want 2: %v", len(result.ChangedAssetIDs), result.ChangedAssetIDs)
	}
}

// --------------- ScanResult carries the seen list the service reconciles with ---------------

func TestScanReturnsSeenList(t *testing.T) {
	root := writeDir(t, t.TempDir(), "root")
	writeFile(t, root, "A.mp4")
	writeFile(t, root, "sub/B.mov")
	writeFile(t, root, "notes.txt") // not seen

	repo := &stubScanRepo{}
	s := NewScanner(repo)

	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Complete || !result.RootReachable {
		t.Fatalf("successful tree result = complete=%v reachable=%v, want true/true", result.Complete, result.RootReachable)
	}
	if len(result.SeenRelativePaths) != 2 {
		t.Fatalf("SeenRelativePaths len = %d, want 2: %v", len(result.SeenRelativePaths), result.SeenRelativePaths)
	}
	// Order should be walk order (fs.WalkDir is lexical).
	seen := result.SeenRelativePaths
	has := func(s string) bool {
		for _, p := range seen {
			if p == s {
				return true
			}
		}
		return false
	}
	if !has("A.mp4") {
		t.Error("seen list missing A.mp4")
	}
	if !has("sub/B.mov") {
		t.Error("seen list missing sub/B.mov")
	}
	if has("notes.txt") {
		t.Error("seen list should not contain non-video notes.txt")
	}
}

// --------------- Context cancelled mid-walk before any file ---------------

func TestScanContextPreCancelled(t *testing.T) {
	root := writeDir(t, t.TempDir(), "root")
	writeFile(t, root, "clip.mp4")

	repo := &stubScanRepo{}
	s := NewScanner(repo)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := s.Scan(ctx, domain.LibraryRoot{ID: "r1", Path: root})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if result.Complete {
		t.Error("cancelled scan must be incomplete")
	}
}

// --------------- Error from repo.UpsertScannedFile does not corrupt result ---------------

func TestScanUpsertErrorStillReturnsSeenList(t *testing.T) {
	root := writeDir(t, t.TempDir(), "root")
	writeFile(t, root, "clip.mp4")

	repo := &stubScanRepo{
		upsertFn: func(ctx context.Context, root domain.LibraryRoot, relativePath, absolutePath string, info fs.FileInfo, fingerprint string) (domain.ScannedFile, error) {
			return domain.ScannedFile{}, fmt.Errorf("insert failed")
		},
	}
	s := NewScanner(repo)

	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.SeenRelativePaths) != 0 {
		t.Errorf("SeenRelativePaths = %v, want empty after failed persist", result.SeenRelativePaths)
	}
	if result.Missing != 0 {
		t.Errorf("Missing = %d, want 0", result.Missing)
	}
	if len(result.Errors) != 1 {
		t.Errorf("Errors len = %d, want 1", len(result.Errors))
	}
	if !strings.HasPrefix(result.Errors[0], "persist file: ") {
		t.Errorf("error = %q, want stable persist file prefix", result.Errors[0])
	}
	if result.Complete {
		t.Error("upsert failure must make scan incomplete")
	}
}

// TestScanReportsSkippedExtensionsBounded pins the skipped-file census: a
// supported .mp4 is discovered, an unsupported .mkv and a sidecar are counted
// with their extension types, and a hostile directory with more than
// maxSkippedExtensionTypes distinct extension types folds the excess into
// SkippedOther instead of growing an unbounded map. The supported list is the
// declaration-ordered set the scanner accepts.
func TestScanReportsSkippedExtensionsBounded(t *testing.T) {
	root := writeDir(t, t.TempDir(), "root")
	writeFile(t, root, "clip.mp4")     // supported → discovered
	writeFile(t, root, "clip.mkv")     // unsupported → skipped, named
	writeFile(t, root, "DJI_0001.srt") // sidecar → skipped, named
	writeFile(t, root, "clip2.mkv")    // same extension → one named type
	// 25 distinct invented extensions: 20 are named, 5 fold into SkippedOther.
	for i := 0; i < maxSkippedExtensionTypes+5; i++ {
		writeFile(t, root, "odd"+string(rune('a'+i))+"."+fmt.Sprintf("x%03d", i))
	}

	repo := &stubScanRepo{}
	s := NewScanner(repo)
	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "r1", Path: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Discovered != 1 {
		t.Fatalf("Discovered = %d, want 1 (only the .mp4)", result.Discovered)
	}
	// .mkv×2 + .srt + 25 invented = 28 skipped files total.
	if result.SkippedFiles != 28 {
		t.Fatalf("SkippedFiles = %d, want 28", result.SkippedFiles)
	}
	if len(result.SkippedExtensions) > maxSkippedExtensionTypes {
		t.Fatalf("SkippedExtensions has %d types, want at most %d", len(result.SkippedExtensions), maxSkippedExtensionTypes)
	}
	// .mkv and .srt must be among the named types.
	named := strings.Join(result.SkippedExtensions, ",")
	if !strings.Contains(named, ".mkv") || !strings.Contains(named, ".srt") {
		t.Fatalf("SkippedExtensions = %v, want .mkv and .srt named", result.SkippedExtensions)
	}
	// 25 invented distinct types: 20 named (the first 20), 5 folded. The first
	// two named slots are .mkv and .srt, so the invented types fill the rest:
	// 25 invented + 2 real = 27 distinct types → 20 named, 7 folded.
	if result.SkippedOther != 7 {
		t.Fatalf("SkippedOther = %d, want 7 (distinct types beyond the first 20)", result.SkippedOther)
	}
	// The supported list is present and declaration-ordered.
	want := strings.Join(supportedVideoExtensions, ",")
	if got := strings.Join(result.SupportedExtensions, ","); got != want {
		t.Fatalf("SupportedExtensions = %v, want %v", got, want)
	}
}

// TestSkippedCensusFoldsDistinctTypesNotFiles pins that SkippedOther counts
// distinct extension types beyond the named 20, never files: 100 files sharing
// one extra extension fold into a single "other type", while the file count
// still reflects every skipped file.
func TestSkippedCensusFoldsDistinctTypesNotFiles(t *testing.T) {
	c := skippedCensus{
		seen:   make(map[string]struct{}, maxSkippedExtensionTypes),
		folded: make(map[string]struct{}, maxSkippedExtensionTypes),
	}
	var result domain.ScanResult
	for i := 0; i < maxSkippedExtensionTypes; i++ {
		c.count(fmt.Sprintf("f.%02d", i), &result)
	}
	for i := 0; i < 100; i++ {
		c.count(fmt.Sprintf("x%d.zzz", i), &result)
	}
	if c.otherTypes != 1 {
		t.Fatalf("folded otherTypes = %d, want 1 (one distinct folded type, not 100 files)", c.otherTypes)
	}
	if result.SkippedFiles != maxSkippedExtensionTypes+100 {
		t.Fatalf("SkippedFiles = %d, want %d", result.SkippedFiles, maxSkippedExtensionTypes+100)
	}
}
