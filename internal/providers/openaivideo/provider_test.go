package openaivideo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	videoproviders "github.com/evjohn-icu/nexusgate/internal/providers/video"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	videoanalysis "github.com/evjohn-icu/nexusgate/internal/domain/video_analysis"
	"github.com/evjohn-icu/nexusgate/internal/providers/common"
)

// A window sized to exactly what MaxInlineVideoBytes reports must not exceed
// the declared ceiling once base64-encoded — that was the bug: the budget was
// compared against pre-encoding bytes while the endpoint enforces the
// encoded, on-wire body. Base64 expands by 4/3, so a raw-byte budget equal to
// the declared ceiling arrives on the wire a third larger than it, which is
// what produced the reported "analyse window 4 of 10 ... HTTP 413".
func TestMaxInlineVideoBytesKeepsTheEncodedRequestUnderTheDeclaredCeiling(t *testing.T) {
	const ceiling = 24 << 20
	p := &Provider{MaxInlineBytes: ceiling}
	budget := p.MaxInlineVideoBytes()
	if budget <= 0 || budget >= ceiling {
		t.Fatalf("MaxInlineVideoBytes() = %d, want a positive value below the %d-byte ceiling it was derived from", budget, ceiling)
	}
	// inputVideoURL base64-encodes the raw bytes with no other transform
	// (encoding/base64.StdEncoding), so this is the same expansion the wire
	// request actually pays. A small, fixed allowance covers the JSON
	// envelope and prompt around the encoded video (see
	// inlineOverheadReserveBytes) — it is not proportional to the budget, so
	// it is asserted separately from the base64 math below.
	encodedVideoBytes := base64.StdEncoding.EncodedLen(int(budget))
	const envelopeAllowance = 8 << 10 // generous headroom for "data:video/mp4;base64," + JSON structure
	if int64(encodedVideoBytes)+envelopeAllowance > ceiling {
		t.Fatalf("a window at the reported budget (%d raw bytes) encodes to %d bytes — plus envelope, that clears the declared %d-byte ceiling the endpoint enforces", budget, encodedVideoBytes, ceiling)
	}

	// A zero/negative MaxInlineBytes must fall back to the same arithmetic
	// applied to defaultMaxInlineBytes, not to the raw default unscaled.
	if got, want := (&Provider{}).MaxInlineVideoBytes(), (&Provider{MaxInlineBytes: defaultMaxInlineBytes}).MaxInlineVideoBytes(); got != want {
		t.Fatalf("zero MaxInlineBytes = %d, want the same as an explicit defaultMaxInlineBytes (%d)", got, want)
	}

	// A ceiling below inlineOverheadReserveBytes has no room for a single byte
	// of video once the envelope is paid for, and must say exactly that. This
	// assertion used to demand a positive number, because InlineVideoBudget
	// reads <= 0 as "this provider has no opinion" and substitutes its own
	// larger fallback — so a plain zero let an operator's tight limit produce
	// a *bigger* effective budget than an unconfigured provider. The answer to
	// that is a distinct sentinel the caller can refuse on, not a fictional
	// one-byte budget that the window splitter rounds back up to a
	// thirty-second window and sends anyway.
	if tiny := (&Provider{MaxInlineBytes: 1 << 10}).MaxInlineVideoBytes(); tiny != videoproviders.NoInlineRoom {
		t.Fatalf("MaxInlineVideoBytes() with a sub-reserve ceiling = %d, want NoInlineRoom (%d) so the caller refuses instead of planning against it", tiny, videoproviders.NoInlineRoom)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProviderSendsVideoURLAndDecodesUnifiedResult(t *testing.T) {
	videoPath := filepath.Join(t.TempDir(), "fixture.mp4")
	if err := os.WriteFile(videoPath, []byte("video-fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		messages := body["messages"].([]any)
		content := messages[0].(map[string]any)["content"].([]any)
		video := content[1].(map[string]any)["video_url"].(map[string]any)["url"].(string)
		if !strings.HasPrefix(video, "data:video/mp4;base64,") {
			t.Fatalf("video URL=%q", video)
		}
		response := `{"choices":[{"message":{"content":"{\"summary\":\"夜晚城市街道\",\"raw_tags\":[\"urban_night\"],\"shots\":[{\"start_ms\":12000,\"end_ms\":18000,\"description\":\"霓虹街道\",\"tags\":[\"urban_night\"]}] }"}}]}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header), Request: r}, nil
	})}
	provider := &Provider{ProviderName: "qwen_video", Endpoint: common.Endpoint{BaseURL: "http://provider.test/v1", HTTPClient: client}, ModelName: "qwen-vl-fixture"}

	result, raw, err := provider.Analyze(context.Background(), videoanalysis.Input{VideoPath: videoPath, MIMEType: "video/mp4"})
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" || result.Summary != "夜晚城市街道" || len(result.Shots) != 1 || result.Shots[0].StartMS != 12000 {
		t.Fatalf("result=%+v raw=%q", result, raw)
	}
}

func TestProviderUsesRemoteVideoURIWhenAvailable(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		content := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
		video := content[1].(map[string]any)["video_url"].(map[string]any)["url"].(string)
		if video != "https://media.example/clip.mp4" {
			t.Fatalf("video URL=%q", video)
		}
		response := `{"choices":[{"message":{"content":"{\"summary\":\"remote clip\"}"}}]}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header), Request: r}, nil
	})}
	provider := &Provider{ProviderName: "volcengine_video", Endpoint: common.Endpoint{BaseURL: "http://provider.test/v1", HTTPClient: client}}
	result, _, err := provider.Analyze(context.Background(), videoanalysis.Input{RemoteURI: "https://media.example/clip.mp4"})
	if err != nil || result.Summary != "remote clip" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

// TestMaxInlineVideoBytesNeverPromisesMoreThanBase64CanFit pins the property
// the whole conversion exists for: whatever MaxInlineVideoBytes hands the
// window splitter, base64-encoding that many raw bytes plus the reserved
// envelope headroom must not exceed the wire ceiling the operator actually
// configured (or the conservative default, when they configured nothing).
// That has to hold at the small end (where usable*3/4 rounds the wrong way)
// and at the large end (where usable*3 overflows int64) alike — a hardcoded
// expected number would not catch either on its own, so every case is
// checked against the ceiling instead.
//
// A safety check alone is not enough: the int64 overflow at math.MaxInt64
// happens to wrap to a budget that is still safe, only wrong (too small by
// billions of bytes). The tightness check below — one more encoded byte must
// not fit — is what catches that, because a too-small budget passes safety
// but fails tightness.
func TestMaxInlineVideoBytesNeverPromisesMoreThanBase64CanFit(t *testing.T) {
	const reserve = inlineOverheadReserveBytes
	cases := []struct {
		name       string
		ceiling    int64
		wantNoRoom bool
	}{
		{"zero ceiling selects the default", 0, false},
		{"ceiling exactly the reserve", reserve, true},
		{"reserve+1", reserve + 1, true},
		{"reserve+2", reserve + 2, true},
		{"reserve+3", reserve + 3, true},
		{"reserve+4 is the first ceiling with any room", reserve + 4, false},
		{"realistic 24MiB", 24 << 20, false},
		{"math.MaxInt64", math.MaxInt64, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Provider{MaxInlineBytes: tc.ceiling}
			got := p.MaxInlineVideoBytes()

			if tc.wantNoRoom {
				if got != videoproviders.NoInlineRoom {
					t.Fatalf("ceiling=%d: got %d, want NoInlineRoom (%d)", tc.ceiling, got, videoproviders.NoInlineRoom)
				}
				return
			}
			if got == videoproviders.NoInlineRoom {
				t.Fatalf("ceiling=%d: reported NoInlineRoom for a ceiling that has room", tc.ceiling)
			}
			if got <= 0 {
				t.Fatalf("ceiling=%d: budget %d is not positive", tc.ceiling, got)
			}

			effectiveCeiling := tc.ceiling
			if effectiveCeiling <= 0 {
				effectiveCeiling = defaultMaxInlineBytes
			}

			// Safety: encoding the returned budget, plus the reserved
			// envelope headroom, must fit inside the ceiling. Subtracting
			// (rather than adding the reserve to the encoded length) avoids
			// its own overflow at a ceiling near math.MaxInt64.
			if encoded := int64(base64.StdEncoding.EncodedLen(int(got))); encoded > effectiveCeiling-reserve {
				t.Fatalf("ceiling=%d: budget %d encodes to %d bytes, which with the %d reserve exceeds the ceiling",
					tc.ceiling, got, encoded, reserve)
			}
			// Tightness: one more raw byte must not have fit. This is what
			// catches the int64-overflow regression at math.MaxInt64, which
			// stays safe but drifts far below the true limit.
			if encoded := int64(base64.StdEncoding.EncodedLen(int(got + 1))); encoded <= effectiveCeiling-reserve {
				t.Fatalf("ceiling=%d: budget %d is not tight — %d would also have fit (encodes to %d, ceiling-reserve=%d)",
					tc.ceiling, got, got+1, encoded, effectiveCeiling-reserve)
			}
		})
	}
}
