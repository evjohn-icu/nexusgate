package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// transcriptEndpointFixture wires a Hub with exactly one scanned asset and
// returns the ready server, its repository and the asset id, so each subtest
// seeds its own transcript state through the real SQLite layer.
func transcriptEndpointFixture(t *testing.T) (*Server, *sqlite.Repository, string) {
	t.Helper()
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "transcript-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rootPath := t.TempDir()
	videoPath := filepath.Join(rootPath, "fixture.mp4")
	if err := os.WriteFile(videoPath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateLibraryRoot(ctx, rootPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(videoPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpsertScannedFile(ctx, root, "fixture.mp4", videoPath, info, "fixture-fingerprint"); err != nil {
		t.Fatal(err)
	}
	assets, err := repo.ListAssets(ctx, 10, 0)
	if err != nil || len(assets) != 1 {
		t.Fatalf("assets=%+v err=%v", assets, err)
	}
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	return NewServer("", service), repo, assets[0].ID
}

func getTranscriptResponse(t *testing.T, server *Server, assetID string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/assets/"+assetID+"/transcript", nil))
	return response
}

// TestAssetTranscriptEndpointThreePaths exercises GET /api/v1/assets/{id}/transcript
// against a real SQLite Hub: aligned carries the word stream, asr-only omits
// words, and an asset with neither answers 404 (not an empty 200).
func TestAssetTranscriptEndpointThreePaths(t *testing.T) {
	t.Run("aligned wins and carries the word stream", func(t *testing.T) {
		server, repo, assetID := transcriptEndpointFixture(t)
		ctx := context.Background()
		if err := repo.SaveTranscript(ctx, assetID, "fixture", "fixture-model", "api-speech", domain.Transcript{Language: "zh", Text: "明天见"}, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := repo.SaveAlignment(ctx, assetID, "fixture", "fixture-model", "api-align", "{}", domain.AlignmentResult{Words: []domain.AlignmentWord{{StartMS: 100, EndMS: 200, Text: "明天"}, {StartMS: 200, EndMS: 300, Text: "见"}}}); err != nil {
			t.Fatal(err)
		}
		response := getTranscriptResponse(t, server, assetID)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var body struct {
			AssetID  string                     `json:"asset_id"`
			Source   string                     `json:"source"`
			Language string                     `json:"language"`
			Text     string                     `json:"text"`
			Segments []domain.TranscriptSegment `json:"segments"`
			Words    []domain.AlignmentWord     `json:"words"`
		}
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Source != "aligned" || body.Language != "zh" || len(body.Words) != 2 || body.Words[0].Text != "明天" || body.Words[0].StartMS != 100 {
			t.Fatalf("body=%+v, want source=aligned with the intact word stream", body)
		}
		// text/segments are promoted from the word stream, not the ASR sentence.
		if body.Text != "明天 见" || len(body.Segments) != 2 || body.Segments[1].StartMS != 200 {
			t.Fatalf("body=%+v, want text/segments promoted from the words", body)
		}
	})

	t.Run("asr-only omits words", func(t *testing.T) {
		server, repo, assetID := transcriptEndpointFixture(t)
		ctx := context.Background()
		if err := repo.SaveTranscript(ctx, assetID, "fixture", "fixture-model", "api-speech", domain.Transcript{Language: "zh", Text: "完整文本", Segments: []domain.TranscriptSegment{{StartMS: 0, EndMS: 1200, Text: "句子"}}}, "", ""); err != nil {
			t.Fatal(err)
		}
		response := getTranscriptResponse(t, server, assetID)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var body struct {
			Source   string                     `json:"source"`
			Language string                     `json:"language"`
			Text     string                     `json:"text"`
			Segments []domain.TranscriptSegment `json:"segments"`
			Words    []domain.AlignmentWord     `json:"words"`
		}
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Source != "asr" || body.Words != nil || body.Text != "完整文本" || len(body.Segments) != 1 {
			t.Fatalf("body=%+v, want source=asr with segments and no words", body)
		}
	})

	t.Run("neither answers 404 not an empty 200", func(t *testing.T) {
		server, _, assetID := transcriptEndpointFixture(t)
		response := getTranscriptResponse(t, server, assetID)
		if response.Code != http.StatusNotFound {
			t.Fatalf("status=%d body=%s, want 404", response.Code, response.Body.String())
		}
		var envelope errorEnvelope
		if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Error.Code != "not_found" {
			t.Fatalf("envelope=%+v, want code not_found", envelope.Error)
		}
	})
}
