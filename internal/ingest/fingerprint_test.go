package ingest

import "testing"

// StableAssetKey is a pure function of content identity — fingerprint, size,
// mtime — and takes no path at all. That is the point: a rename keeps all
// three inputs intact, so the key must too.

func TestStableAssetKeySameFieldsStable(t *testing.T) {
	key := StableAssetKey("fp-x", 1024, 123456789)
	for i := 0; i < 3; i++ {
		if got := StableAssetKey("fp-x", 1024, 123456789); got != key {
			t.Fatalf("StableAssetKey changed for identical inputs: got %q, want %q", got, key)
		}
	}
}

// The rename/move scenario: os.Rename preserves content, size and mtime, so
// the key must come out identical no matter which path the file sits at. The
// function cannot even be asked to differ on path — it has no path input.
func TestStableAssetKeyRenamedFileSameKey(t *testing.T) {
	before := StableAssetKey("fp-x", 1024, 99)
	after := StableAssetKey("fp-x", 1024, 99)
	if before != after {
		t.Fatalf("a pure path change altered the key: %q vs %q", before, after)
	}
}

func TestStableAssetKeyDifferentMtimeDifferentKey(t *testing.T) {
	if StableAssetKey("fp-x", 1024, 1) == StableAssetKey("fp-x", 1024, 2) {
		t.Fatal("a different mtime must produce a different key (a real edit re-runs the chain)")
	}
}

func TestStableAssetKeyDifferentFingerprintDifferentKey(t *testing.T) {
	if StableAssetKey("fp-a", 1024, 99) == StableAssetKey("fp-b", 1024, 99) {
		t.Fatal("a different fingerprint must produce a different key")
	}
}

func TestStableAssetKeyDifferentSizeDifferentKey(t *testing.T) {
	if StableAssetKey("fp-x", 1024, 99) == StableAssetKey("fp-x", 2048, 99) {
		t.Fatal("a different size must produce a different key")
	}
}

func TestStableAssetKeyIsHexSha256(t *testing.T) {
	key := StableAssetKey("fp-x", 1024, 99)
	if len(key) != 64 {
		t.Fatalf("StableAssetKey() = %q, want a 64-char hex sha256", key)
	}
	for _, c := range key {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("StableAssetKey() = %q is not hex", key)
		}
	}
}
