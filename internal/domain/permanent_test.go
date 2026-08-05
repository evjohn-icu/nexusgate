package domain

import (
	"errors"
	"fmt"
	"testing"
)

// TestPermanentIsAPropertyNotAReplacement pins the three things every caller
// of Permanent relies on, because a marker that quietly ate part of the chain
// would be worse than the substring list it replaced: it would break the
// status-code classification that already works.
func TestPermanentIsAPropertyNotAReplacement(t *testing.T) {
	inner := errors.New("shot description is required at ordinal 3")
	marked := Permanent(inner)

	if !errors.Is(marked, ErrPermanentFailure) {
		t.Fatal("Permanent(err) must match ErrPermanentFailure")
	}
	if !errors.Is(marked, inner) {
		t.Fatal("marking must not hide what it wrapped")
	}
	if marked.Error() != inner.Error() {
		t.Fatalf("marking rewrote the operator's message: %q became %q", inner.Error(), marked.Error())
	}
	// jobs.last_error_message is written from whatever the pipeline wraps
	// around this on the way up, so the marker has to survive wrapping.
	if !errors.Is(fmt.Errorf("analyze asset: %w", marked), ErrPermanentFailure) {
		t.Fatal("a wrapped permanent failure stopped being permanent")
	}
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil) must stay nil; a nil error is not a failure")
	}
}

// TestPermanentIsOptIn is the half of the contract that protects everything
// nobody thought about. Retrying is the default, and it must not be reachable
// by wording: a message that reads permanent is still retried, because the
// alternative -- deciding from prose -- is what allowed an upstream 5xx body
// echoing "not configured" to be declared final.
func TestPermanentIsOptIn(t *testing.T) {
	for _, err := range []error{
		errors.New("provider is not configured"),
		errors.New("unsupported pixel format"),
		errors.New("permanent failure"),
	} {
		if errors.Is(err, ErrPermanentFailure) {
			t.Fatalf("an unmarked error was treated as permanent on its wording alone: %v", err)
		}
	}
}

// A sentinel that is itself marked keeps its own identity. ErrProviderChannelNotConfigured
// (internal/app) is declared this way so the condition is named once and
// gains permanence rather than acquiring a second name for the same thing.
func TestMarkedSentinelKeepsItsOwnIdentity(t *testing.T) {
	sentinel := Permanent(errors.New("provider channel capability is not configured"))

	if !errors.Is(sentinel, sentinel) {
		t.Fatal("a marked sentinel must still match itself")
	}
	if !errors.Is(fmt.Errorf("resolve capability: %w", sentinel), sentinel) {
		t.Fatal("callers must still recognise the sentinel through wrapping")
	}
	if errors.Is(Permanent(errors.New("something else")), sentinel) {
		t.Fatal("two separately marked errors must not be the same condition")
	}
}
