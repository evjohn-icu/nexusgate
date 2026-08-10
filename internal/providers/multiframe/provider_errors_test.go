package multiframe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	videoanalysis "github.com/evjohn-icu/timingdex/internal/domain/video_analysis"
	"github.com/evjohn-icu/timingdex/internal/providers/common"
	videoproviders "github.com/evjohn-icu/timingdex/internal/providers/video"
)

func TestProviderErrorFixtures(t *testing.T) {
	const key = "multiframe-echo-key"
	frameDir := t.TempDir()
	frame := writeFrame(t, frameDir, "fixture.jpg", 100)
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"HTTP status", http.StatusBadGateway, strings.Repeat("upstream ", 500) + key, "upstream"},
		{"malformed 2xx", http.StatusOK, strings.Repeat("malformed ", 500) + key, "missing choices"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer s.Close()
			p := &Provider{Endpoint: common.Endpoint{BaseURL: s.URL, APIKey: key}}
			_, raw, err := p.AnalyzeShot(context.Background(), videoproviders.ShotAnalysisRequest{Frames: []videoanalysis.Frame{frame}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), key) || strings.Contains(raw, key) {
				t.Fatalf("key leaked: err=%q raw=%q", err, raw)
			}
			if len(err.Error()) > 2048+128 || len(raw) > 2048+128 {
				t.Fatalf("error/raw not bounded: %d/%d", len(err.Error()), len(raw))
			}
			if tc.status != http.StatusOK {
				var se *common.StatusError
				if !errors.As(err, &se) || se.HTTPStatusCode() != tc.status {
					t.Fatalf("status not preserved: %T %v", err, err)
				}
				if strings.Contains(se.Body, key) || strings.Contains(se.Error(), key) || len(se.Body) > 2048+len("…(truncated)") {
					t.Fatalf("status body unsafe: %q", se.Body)
				}
			}
		})
	}
}
