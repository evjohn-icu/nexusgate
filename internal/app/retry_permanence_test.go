package app

import (
	"errors"
	"fmt"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/normalize"
	"github.com/evjohn-icu/nexusgate/internal/providerchannels"
)

// TestModelOutputValidationIsNeverRetried is the assertion the phrase list
// could not make. Every error below is a verdict on bytes the Hub already
// holds: the provider was paid, answered, and the answer failed a
// deterministic check. Asking the same model the same question again returns
// the same unusable answer, so a retry is a second charge for a known
// outcome.
//
// The errors are produced by calling the real validators rather than by
// typing their messages here, so rewording a message cannot make this test
// pass by accident -- which is exactly what a substring classifier would let
// it do.
func TestModelOutputValidationIsNeverRetried(t *testing.T) {
	shotCases := []struct {
		name       string
		shots      []domain.AssetShot
		durationMS int64
	}{
		{"shot count over the cap", overLimitShots(), 60000},
		{"shot time range inverted", []domain.AssetShot{{StartMS: 4000, EndMS: 1000, Description: "a shot"}}, 60000},
		{"shot description blank", []domain.AssetShot{{StartMS: 0, EndMS: 1000, Description: "   "}}, 60000},
		{"shot ends after the asset", []domain.AssetShot{{StartMS: 0, EndMS: 90000, Description: "a shot"}}, 60000},
	}
	for _, tc := range shotCases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAnalysisShots(tc.shots, tc.durationMS)
			if err == nil {
				t.Fatal("validateAnalysisShots accepted output it should reject")
			}
			if isRetryableJobError(err) {
				t.Fatalf("rejected model output was classified retryable (%v); every retry is another paid call for the same answer", err)
			}
		})
	}

	if _, err := normalize.ValidateAndNormalize(domain.StructuredAnalysis{}); err == nil {
		t.Fatal("ValidateAndNormalize accepted an analysis with no summary")
	} else if isRetryableJobError(err) {
		t.Fatalf("rejected analysis was classified retryable (%v)", err)
	}
}

// TestProviderChannelConfigurationFailuresAreNotRetried covers the other
// family the phrase list carried: a route that names something this Hub
// cannot build. The builder is called for real rather than asserted against a
// typed message, so the marking wrapper -- not a sentence -- is what is being
// tested, and a `case` added to that switch is covered without anyone
// touching this file.
func TestProviderChannelConfigurationFailuresAreNotRetried(t *testing.T) {
	runtime := &providerChannelRuntime{}
	invocation := providerchannels.Invocation{ProviderName: "no-such-provider"}

	if _, err := runtime.asrProvider(invocation, "key"); err == nil {
		t.Fatal("an unknown ASR provider name was accepted")
	} else if isRetryableJobError(err) {
		t.Fatalf("an unbuildable ASR route was classified retryable (%v)", err)
	}
	if _, err := runtime.videoProvider(invocation, "key"); err == nil {
		t.Fatal("an unknown video provider name was accepted")
	} else if isRetryableJobError(err) {
		t.Fatalf("an unbuildable video route was classified retryable (%v)", err)
	}

	for _, err := range []error{ErrProviderChannelNotConfigured, errProviderChannelSecretMissing} {
		if isRetryableJobError(err) {
			t.Fatalf("a configuration gap was classified retryable (%v)", err)
		}
	}
	// Marking ErrProviderChannelNotConfigured must not have cost it the
	// identity service.go already matches on.
	if !errors.Is(fmt.Errorf("draft plan: %w", ErrProviderChannelNotConfigured), ErrProviderChannelNotConfigured) {
		t.Fatal("marking the sentinel broke the errors.Is match service.go depends on")
	}
}

// TestProviderChannelMultiframeProtocolRejected pins the fail-fast for
// openai_multiframe channels: channel routing has no multiframe
// orchestration, so building the provider must refuse with the documented
// boundary sentence instead of letting the pipeline reach "multiframe
// summary call requires at least one frame" on the first frame-less call.
// The refusal is permanent (it is a verdict on the channel's own metadata),
// and the plain video protocol must remain buildable.
func TestProviderChannelMultiframeProtocolRejected(t *testing.T) {
	runtime := &providerChannelRuntime{}

	multiframe := providerchannels.Invocation{
		ProviderName: "local_vlm",
		Protocol:     "openai_multiframe",
		Endpoint:     "http://127.0.0.1:8080/v1",
		Path:         "chat/completions",
		Model:        "Qwen3-VL-4B-Instruct",
	}
	if _, err := runtime.videoProvider(multiframe, "key"); err == nil {
		t.Fatal("a local_vlm openai_multiframe channel was accepted without multiframe orchestration")
	} else {
		if isRetryableJobError(err) {
			t.Fatalf("an unbuildable multiframe route was classified retryable (%v)", err)
		}
		if err.Error() != "openai_multiframe is currently supported through providers.local_vlm config only; provider-channel routing support is not available yet" {
			t.Fatalf("unexpected refusal message: %v", err)
		}
	}

	video := providerchannels.Invocation{
		ProviderName: "local_vlm",
		Protocol:     "openai_video",
		Endpoint:     "http://127.0.0.1:8080/v1",
		Path:         "chat/completions",
		Model:        "Qwen3-VL-4B-Instruct",
	}
	// The zero-config runtime cannot build any real provider (it reports the
	// channel disabled) — the control here is only that the multiframe guard
	// is protocol-specific and did not fire for the plain video protocol.
	if _, err := runtime.videoProvider(video, "key"); err == errProviderChannelMultiframeUnsupported {
		t.Fatalf("plain openai_video was refused by the multiframe guard: %v", err)
	}
}

func overLimitShots() []domain.AssetShot {
	shots := make([]domain.AssetShot, maxAnalysisShots+1)
	for i := range shots {
		shots[i] = domain.AssetShot{StartMS: 0, EndMS: 1000, Description: "a shot"}
	}
	return shots
}
