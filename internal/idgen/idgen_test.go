package idgen

import (
	"testing"
)

func TestNew_UUIDFormat(t *testing.T) {
	id := New()
	if len(id) != 36 {
		t.Fatalf("expected length 36, got %d: %q", len(id), id)
	}
	// Dashes at positions 8, 13, 18, 23.
	for _, pos := range []int{8, 13, 18, 23} {
		if id[pos] != '-' {
			t.Fatalf("expected dash at position %d, got %q", pos, id[pos])
		}
	}
	// All other characters must be hex.
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("expected hex at position %d, got %q", i, c)
		}
	}
}

func TestNew_UUIDVersionVariant(t *testing.T) {
	id := New()
	// Position 14 is the version nibble.
	if id[14] != '4' {
		t.Errorf("expected version nibble '4' at position 14, got %q", id[14])
	}
	// Position 19 is the variant nibble (0x80–0xbf → '8','9','a','b').
	switch id[19] {
	case '8', '9', 'a', 'b':
	default:
		t.Errorf("expected variant nibble in {8,9,a,b} at position 19, got %q", id[19])
	}
}

func TestNew_Uniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		id := New()
		if seen[id] {
			t.Fatalf("duplicate UUID generated on iteration %d: %s", i, id)
		}
		seen[id] = true
	}
}
