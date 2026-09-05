package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/app"
	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
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
		if err := repo.SaveAlignment(ctx, assetID, "fixture", "fixture-model", "api-align", "{}", domain.AlignmentResult{Words: []domain.AlignmentWord{{StartMS: 100, EndMS: 200, Text: "明天"}, {StartMS: 200, EndMS: 300, Text: "见"}}}, "", ""); err != nil {
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

// TestAssetDetailRedactsDerivedArtifactPaths pins that the public asset detail
// response carries no absolute on-disk path. thumbnail_path/proxy_path were
// filled with the artifact's LocalPath in the repository and never cleared by
// the handler's redaction block — which two lines above promises absolute
// paths stay behind the administrator boundary — so every trusted-read caller
// (any LAN peer, any agent token from any network) received the Hub's data
// directory layout, operator username included. Found while investigating
// M1-05; the browser fixture could not demonstrate it because its seeded asset
// has no derived artifacts, so this test makes some.
func TestAssetDetailRedactsDerivedArtifactPaths(t *testing.T) {
	server, repo, assetID := transcriptEndpointFixture(t)
	ctx := context.Background()

	// SaveArtifact is lease-bound, so the artifacts have to arrive the way the
	// pipeline delivers them: through a held lease on a running job.
	if err := repo.EnqueueJob(ctx, assetID, domain.JobDerive, "detail-redaction-hash", 0); err != nil {
		t.Fatal(err)
	}
	job, err := repo.LeaseNextJob(ctx, "owner-a", func(domain.JobType) time.Duration { return time.Hour }, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("job=%v err=%v", job, err)
	}
	secret := "/home/someone/.nexusgate-dev/cache/derived/" + assetID
	for _, a := range []domain.DerivedArtifact{
		{ID: "art-thumb", AssetID: assetID, Type: "thumbnail", LocalPath: secret + "/thumbnail-software.jpg", SizeBytes: 1},
		{ID: "art-proxy", AssetID: assetID, Type: "proxy", LocalPath: secret + "/proxy-software.mp4", SizeBytes: 2},
	} {
		if err := repo.SaveArtifact(ctx, a, job.ID, "owner-a"); err != nil {
			t.Fatal(err)
		}
	}

	// The repository still reports them — the boundary is the handler's, and
	// nothing else in the tree reads these fields.
	detail, err := repo.GetAssetDetail(ctx, assetID)
	if err != nil || detail == nil {
		t.Fatalf("detail=%v err=%v", detail, err)
	}
	if detail.ThumbnailPath == "" || detail.ProxyPath == "" {
		t.Fatal("fixture did not actually attach artifacts; the test would pass vacuously")
	}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/assets/"+assetID, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if strings.Contains(body, secret) {
		t.Fatalf("asset detail leaks an absolute derived-artifact path: %s", body)
	}
	var decoded struct {
		ThumbnailPath string `json:"thumbnail_path"`
		ProxyPath     string `json:"proxy_path"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ThumbnailPath != "" || decoded.ProxyPath != "" {
		t.Fatalf("thumbnail_path=%q proxy_path=%q, want both absent", decoded.ThumbnailPath, decoded.ProxyPath)
	}
}
