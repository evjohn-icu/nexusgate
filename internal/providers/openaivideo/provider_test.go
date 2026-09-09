package openaivideo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
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

	// A ceiling below inlineOverheadReserveBytes must still report a positive
	// budget. videoproviders.InlineVideoBudget reads <= 0 as "this provider
	// has no opinion" and substitutes its own, larger fallback — so a zero
	// here would let an operator's explicit, unrealistically tight limit
	// produce a *bigger* effective budget than an unconfigured provider,
	// exactly backwards.
	if tiny := (&Provider{MaxInlineBytes: 1 << 10}).MaxInlineVideoBytes(); tiny <= 0 {
		t.Fatalf("MaxInlineVideoBytes() with a sub-reserve ceiling = %d, want a positive value so InlineVideoBudget never reads it as unknown", tiny)
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
