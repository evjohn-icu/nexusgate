package providerpool

import (
	"fmt"
	"testing"
)

// internal/providers/volcasr speaks WebSocket, not HTTP: it has no status to
// attach, so on its error-frame path (asr.go's fmt.Errorf("Volcengine ASR
// error: %s", ...) fed the raw upstream payload) it forwards the provider's
// text verbatim into the error this package classifies. Volcengine's own
// error codes are eight digits with no HTTP-status meaning at all, but a
// code like 45000002 contains "500" as a plain substring. This reproduces
// that exact shape -- a status-free error whose text contains a number that
// merely looks like a 5xx status -- without depending on volcasr or the
// network.
func TestVolcengineStyleErrorCodeIsNotMistakenForAServerStatus(t *testing.T) {
	err := fmt.Errorf("Volcengine ASR error: %s", `{"code":45000002,"message":"appid not registered"}`)
	if got := ClassifyFailure(err); got == Retryable {
		t.Fatalf("ClassifyFailure(%v) = %v, want NonRetryable -- \"45000002\" contains \"500\" but names no status and no known permanent rejection burned the full backoff for it", err, got)
	}
}

// The result must not depend on which digits happen to appear. A different
// Volcengine code, or one chosen to also contain a 4xx-looking run, must land
// the same way: this is a status-free error and no digit run in it names
// anything.
func TestStatusFreeErrorClassificationIgnoresWhichDigitsAppear(t *testing.T) {
	for _, code := range []string{"45000002", "50000001", "40122903", "70000009"} {
		err := fmt.Errorf("Volcengine ASR error: %s", fmt.Sprintf(`{"code":%s,"message":"appid not registered"}`, code))
		if got := ClassifyFailure(err); got != NonRetryable {
			t.Fatalf("ClassifyFailure with embedded code %q = %v, want NonRetryable in every case", code, got)
		}
	}
}
