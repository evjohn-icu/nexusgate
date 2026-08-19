package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// TestSearchSpeechViaAsrOnlyEndpoint exercises the end-to-end speech claim for
// an asset whose timing comes only from the ASR transcript (no forced
// alignment): POST /api/v1/search/shots in speech mode pinpoints the shot and
// reports the speech phrase as possible/transcript evidence.
func TestSearchSpeechViaAsrOnlyEndpoint(t *testing.T) {
	server, repo, assetID := transcriptEndpointFixture(t)
	ctx := context.Background()
	if err := repo.ReplaceAssetShots(ctx, assetID, "", []domain.AssetShot{{ID: "shot-1", StartMS: 0, EndMS: 5000, Description: "interview"}}, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveTranscript(ctx, assetID, "stepfun", "stepfun-asr", "api-asr-1", domain.Transcript{
		Language: "zh",
		Text:     "我们明天出发去上海",
		Segments: []domain.TranscriptSegment{{StartMS: 100, EndMS: 200, Text: "我们明天出发去上海"}},
	}, "", ""); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, lanRequest(http.MethodPost, "/api/v1/search/shots", strings.NewReader(`{"query":"他说过我们明天出发去上海","mode":"speech","limit":10,"include_evidence":true}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var parsed struct {
		Results []struct {
			ShotID string `json:"shot_id"`
			Evidence []struct {
				ConstraintType string   `json:"constraint_type"`
				Constraint     string   `json:"constraint"`
				State          string   `json:"state"`
				Sources        []string `json:"sources"`
			} `json:"evidence"`
		} `json:"results"`
	}
	if err := json.NewDecoder(response.Body).Decode(&parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Results) != 1 || parsed.Results[0].ShotID != "shot-1" {
		t.Fatalf("results=%+v, want shot-1", parsed.Results)
	}
	found := false
	for _, e := range parsed.Results[0].Evidence {
		if e.ConstraintType == "speech" && e.Constraint == "我们明天出发去上海" && e.State == "possible" {
			for _, src := range e.Sources {
				if src == "transcript" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("missing ASR speech evidence: %+v", parsed.Results[0].Evidence)
	}
}
