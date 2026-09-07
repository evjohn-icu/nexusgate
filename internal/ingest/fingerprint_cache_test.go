package ingest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// The tests below all turn on one trick: the file's bytes are replaced while
// its size and mtime are held constant. Nothing else can distinguish "the
// scanner reused the stored fingerprint" from "the scanner read the file and
// happened to get the same answer" — a counter on the repository would only
// prove the cache was consulted, not that the read was skipped. The identity
// the scanner hands to UpsertScannedFile is the observation: if it is the old
// fingerprint, the new bytes were never opened.
func rewriteHoldingStat(t *testing.T, path string, content []byte) domain.KnownFile {
	t.Helper()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(content)) != before.Size() {
		t.Fatalf("replacement content is %d bytes, original is %d — the size must not\nchange or the cache would miss for a reason this test is not about", len(content), before.Size())
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Read the mtime back rather than reusing the one just written: some
	// filesystems store fewer than nanoseconds, and the cache entry has to
	// hold what a previous scan would actually have persisted.
	return domain.KnownFile{Size: after.Size(), ModifiedNS: after.ModTime().UnixNano()}
}

func fingerprintCacheFixture(t *testing.T) (root domain.LibraryRoot, oldPrint, newPrint string, entry domain.KnownFile) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "clip.mov")
	if err := os.WriteFile(path, []byte("original-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	oldPrint, err = QuickFingerprint(path, info.Size())
	if err != nil {
		t.Fatal(err)
	}
	entry = rewriteHoldingStat(t, path, []byte("replaced-bytes"))
	entry.Fingerprint = oldPrint
	newPrint, err = QuickFingerprint(path, entry.Size)
	if err != nil {
		t.Fatal(err)
	}
	// Positive control. If the two contents fingerprinted the same, every
	// assertion below would hold no matter what the scanner did.
	if oldPrint == newPrint {
		t.Fatalf("the two contents fingerprint identically (%s), so this test\ncannot tell a reused fingerprint from a fresh read", oldPrint)
	}
	return domain.LibraryRoot{ID: "root-1", Path: dir}, oldPrint, newPrint, entry
}

func TestScanReusesTheStoredFingerprintWithoutReadingTheFile(t *testing.T) {
	root, oldPrint, newPrint, entry := fingerprintCacheFixture(t)
	repo := &stubScanRepo{known: map[string]domain.KnownFile{"clip.mov": entry}}

	if _, err := NewScanner(repo).Scan(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if len(repo.fingerprints) != 1 {
		t.Fatalf("upserted %d files, want 1", len(repo.fingerprints))
	}
	if repo.fingerprints[0] != oldPrint {
		if repo.fingerprints[0] == newPrint {
			t.Fatal("the scanner re-read the file: it handed over the fingerprint of the\nnew bytes, so the stored one was ignored and every scan still pays\n12 MiB per file")
		}
		t.Fatalf("fingerprint = %s, want the stored %s", repo.fingerprints[0], oldPrint)
	}
	if repo.knownCalls != 1 {
		t.Fatalf("consulted the cache %d times for one file, want 1", repo.knownCalls)
	}
}

func TestScanRereadsWhenTheMtimeMoved(t *testing.T) {
	root, _, newPrint, entry := fingerprintCacheFixture(t)
	// One nanosecond is enough: the comparison is equality, not a window.
	entry.ModifiedNS++
	repo := &stubScanRepo{known: map[string]domain.KnownFile{"clip.mov": entry}}

	if _, err := NewScanner(repo).Scan(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if repo.fingerprints[0] != newPrint {
		t.Fatalf("fingerprint = %s, want the freshly read %s — a moved mtime is the\nsignal that the bytes changed, and reusing the stored identity there\nwould leave the pipeline deriving from content that no longer exists", repo.fingerprints[0], newPrint)
	}
}

func TestScanRereadsWhenTheSizeMoved(t *testing.T) {
	root, _, newPrint, entry := fingerprintCacheFixture(t)
	entry.Size++
	repo := &stubScanRepo{known: map[string]domain.KnownFile{"clip.mov": entry}}

	if _, err := NewScanner(repo).Scan(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if repo.fingerprints[0] != newPrint {
		t.Fatalf("fingerprint = %s, want the freshly read %s", repo.fingerprints[0], newPrint)
	}
}

func TestScanDeepIgnoresTheCacheEntirely(t *testing.T) {
	root, oldPrint, newPrint, entry := fingerprintCacheFixture(t)
	repo := &stubScanRepo{known: map[string]domain.KnownFile{"clip.mov": entry}}

	if _, err := NewScanner(repo).ScanDeep(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if repo.fingerprints[0] != newPrint {
		t.Fatalf("fingerprint = %s, want the freshly read %s — --deep exists for exactly\nthis case (content rewritten with the mtime restored) and is worthless\nif it still trusts the entry", repo.fingerprints[0], newPrint)
	}
	if repo.fingerprints[0] == oldPrint {
		t.Fatal("ScanDeep returned the cached fingerprint")
	}
	if repo.knownCalls != 0 {
		t.Fatalf("ScanDeep consulted the cache %d times, want 0", repo.knownCalls)
	}
}

func TestScanTreatsACacheLookupFailureAsAMiss(t *testing.T) {
	root, _, newPrint, _ := fingerprintCacheFixture(t)
	repo := &stubScanRepo{knownErr: errors.New("database is locked")}

	result, err := NewScanner(repo).Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if repo.fingerprints[0] != newPrint {
		t.Fatalf("fingerprint = %s, want the freshly read %s", repo.fingerprints[0], newPrint)
	}
	// The scan must stay complete. An entry in Errors would flip Complete to
	// false, and internal/app reads that as "the root may not be reachable" —
	// so a failing cache read would stop reconciliation for the whole root.
	if len(result.Errors) != 0 {
		t.Fatalf("a failed cache lookup was recorded as a scan error: %v", result.Errors)
	}
	if !result.Complete {
		t.Fatal("a failed cache lookup marked the scan incomplete, which blocks the\nreconciliation gate in internal/app for the entire root")
	}
}

func TestScanReadsWhenTheCacheHasNoEntry(t *testing.T) {
	root, _, newPrint, _ := fingerprintCacheFixture(t)
	repo := &stubScanRepo{}

	if _, err := NewScanner(repo).Scan(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if repo.fingerprints[0] != newPrint {
		t.Fatalf("fingerprint = %s, want the freshly read %s", repo.fingerprints[0], newPrint)
	}
	if repo.knownCalls != 1 {
		t.Fatalf("consulted the cache %d times, want 1", repo.knownCalls)
	}
}

func TestScanReadsWhenTheStoredFingerprintIsEmpty(t *testing.T) {
	root, _, newPrint, entry := fingerprintCacheFixture(t)
	entry.Fingerprint = ""
	repo := &stubScanRepo{known: map[string]domain.KnownFile{"clip.mov": entry}}

	if _, err := NewScanner(repo).Scan(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if repo.fingerprints[0] != newPrint {
		t.Fatalf("fingerprint = %s, want the freshly read %s — an empty stored identity\nis not an identity, and handing it to UpsertScannedFile would collapse\nevery such file onto one asset row", repo.fingerprints[0], newPrint)
	}
}
