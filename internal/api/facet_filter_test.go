package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// TestParseFacetFilterValidatesAgainstTheVocabulary is a table-driven test of
// the query-parameter parsing shared by the browse and shot-search handlers.
// Validation happens here, at the HTTP boundary, because normalize's
// exported *Values lists are the only source of truth and domain.FacetFilter
// itself cannot import normalize (normalize imports domain).
func TestParseFacetFilterValidatesAgainstTheVocabulary(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		wantErr   string // substring expected in the error, or "" for no error
		checkGood func(t *testing.T, f domain.FacetFilter)
	}{
		{
			name:  "empty query matches everything (pins today's behavior)",
			query: "",
			checkGood: func(t *testing.T, f domain.FacetFilter) {
				if len(f.AssetTypes) != 0 || len(f.ShotSizes) != 0 || len(f.CameraMotions) != 0 ||
					len(f.AudioTypes) != 0 || len(f.Qualities) != 0 || len(f.UsableAs) != 0 ||
					f.MinDurationMS != nil || f.MaxDurationMS != nil {
					t.Fatalf("empty query produced non-zero filter: %+v", f)
				}
			},
		},
		{
			name:  "one facet one value",
			query: "shot_size=wide",
			checkGood: func(t *testing.T, f domain.FacetFilter) {
				if len(f.ShotSizes) != 1 || f.ShotSizes[0] != "wide" {
					t.Fatalf("ShotSizes=%v, want [wide]", f.ShotSizes)
				}
			},
		},
		{
			name:  "one facet two values is OR (comma separated)",
			query: "shot_size=wide,medium",
			checkGood: func(t *testing.T, f domain.FacetFilter) {
				if len(f.ShotSizes) != 2 || f.ShotSizes[0] != "wide" || f.ShotSizes[1] != "medium" {
					t.Fatalf("ShotSizes=%v, want [wide medium]", f.ShotSizes)
				}
			},
		},
		{
			name:  "two different facets both parsed (AND is the caller's SQL, not this function)",
			query: "shot_size=wide,medium&camera_motion=static",
			checkGood: func(t *testing.T, f domain.FacetFilter) {
				if len(f.ShotSizes) != 2 || len(f.CameraMotions) != 1 || f.CameraMotions[0] != "static" {
					t.Fatalf("f=%+v, want ShotSizes=[wide medium] CameraMotions=[static]", f)
				}
			},
		},
		{
			name:  "usable_as accepts a value from its list",
			query: "usable_as=establishing,hook",
			checkGood: func(t *testing.T, f domain.FacetFilter) {
				if len(f.UsableAs) != 2 {
					t.Fatalf("UsableAs=%v, want 2 values", f.UsableAs)
				}
			},
		},
		{
			name:  "duration bounds parse as int64",
			query: "min_duration_ms=4000&max_duration_ms=8000",
			checkGood: func(t *testing.T, f domain.FacetFilter) {
				if f.MinDurationMS == nil || *f.MinDurationMS != 4000 {
					t.Fatalf("MinDurationMS=%v, want 4000", f.MinDurationMS)
				}
				if f.MaxDurationMS == nil || *f.MaxDurationMS != 8000 {
					t.Fatalf("MaxDurationMS=%v, want 8000", f.MaxDurationMS)
				}
			},
		},
		{
			name:  "equal duration bounds are a valid exact-duration query",
			query: "min_duration_ms=4000&max_duration_ms=4000",
			checkGood: func(t *testing.T, f domain.FacetFilter) {
				if f.MinDurationMS == nil || *f.MinDurationMS != 4000 {
					t.Fatalf("MinDurationMS=%v, want 4000", f.MinDurationMS)
				}
				if f.MaxDurationMS == nil || *f.MaxDurationMS != 4000 {
					t.Fatalf("MaxDurationMS=%v, want 4000", f.MaxDurationMS)
				}
			},
		},
		{
			name:  "only a min duration bound is accepted",
			query: "min_duration_ms=4000",
			checkGood: func(t *testing.T, f domain.FacetFilter) {
				if f.MinDurationMS == nil || *f.MinDurationMS != 4000 {
					t.Fatalf("MinDurationMS=%v, want 4000", f.MinDurationMS)
				}
				if f.MaxDurationMS != nil {
					t.Fatalf("MaxDurationMS=%v, want nil", f.MaxDurationMS)
				}
			},
		},
		{
			name:  "only a max duration bound is accepted",
			query: "max_duration_ms=8000",
			checkGood: func(t *testing.T, f domain.FacetFilter) {
				if f.MinDurationMS != nil {
					t.Fatalf("MinDurationMS=%v, want nil", f.MinDurationMS)
				}
				if f.MaxDurationMS == nil || *f.MaxDurationMS != 8000 {
					t.Fatalf("MaxDurationMS=%v, want 8000", f.MaxDurationMS)
				}
			},
		},
		{
			name:    "invalid shot_size value is rejected and the error names the field",
			query:   "shot_size=extremely_wide",
			wantErr: "shot_size",
		},
		{
			name:    "invalid camera_motion value is rejected and the error names the field",
			query:   "camera_motion=spin",
			wantErr: "camera_motion",
		},
		{
			name:    "invalid usable_as value is rejected and the error names the field",
			query:   "usable_as=hook,not_a_real_value",
			wantErr: "usable_as",
		},
		{
			name:    "non-numeric min_duration_ms is rejected",
			query:   "min_duration_ms=soon",
			wantErr: "min_duration_ms",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			values, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatalf("test query %q does not parse: %v", tc.query, err)
			}
			f, err := parseFacetFilter(values)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("query=%q: want error containing %q, got nil", tc.query, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("query=%q: error=%q, want it to name field %q", tc.query, err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("query=%q: unexpected error: %v", tc.query, err)
			}
			tc.checkGood(t, f)
		})
	}
}

// An inverted duration range is the same class of bug as a misspelled facet
// value — it parses, produces SQL that matches nothing, and reads as "you have
// no footage" — so it must be rejected with a message that names both bounds
// and their values, not silently accepted into an empty result.
func TestParseFacetFilterRejectsInvertedDurationRange(t *testing.T) {
	values, err := url.ParseQuery("min_duration_ms=9000&max_duration_ms=3000")
	if err != nil {
		t.Fatal(err)
	}
	_, err = parseFacetFilter(values)
	if err == nil {
		t.Fatal("an inverted duration range must be rejected, not silently produce an empty result")
	}
	for _, want := range []string{"min_duration_ms", "9000", "max_duration_ms", "3000"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error=%q, want it to name %q", err.Error(), want)
		}
	}
}

// newFacetTestService gives the handler-level tests below a real sqlite
// repository and two assets whose asset_analysis rows differ in shot_size
// and camera_motion, wired up through app.Service exactly as production
// does — this is the compile-time and behavioral proof that server.go's
// query parsing actually reaches ListAssetCardsFiltered/SearchShotsFiltered,
// not just that parseFacetFilter itself works in isolation.
func newFacetTestService(t *testing.T) *app.Service {
	t.Helper()
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "facet-handler.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rootPath := t.TempDir()
	root, err := repo.CreateLibraryRoot(ctx, rootPath)
	if err != nil {
		t.Fatal(err)
	}
	// wantID is the asset caller code refers to by name below; UpsertScannedFile
	// only reports whether it created a row, so each fixture's real asset id is
	// recovered by diffing ListAssets before/after — one new asset per fixture.
	seen := map[string]bool{}
	for _, fx := range []struct {
		wantID, assetType, shotSize, cameraMotion string
	}{
		{wantID: "asset-wide", assetType: "b_roll", shotSize: "wide", cameraMotion: "static"},
		{wantID: "asset-closeup", assetType: "talking_to_camera", shotSize: "close_up", cameraMotion: "handheld"},
	} {
		relPath := fx.wantID + ".mp4"
		absPath := filepath.Join(rootPath, relPath)
		if err := os.WriteFile(absPath, []byte("fixture-"+fx.wantID), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(absPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.UpsertScannedFile(ctx, root, relPath, absPath, info, "fp-"+fx.wantID); err != nil {
			t.Fatal(err)
		}
		assets, err := repo.ListAssets(ctx, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		var assetID string
		for _, a := range assets {
			if !seen[a.ID] {
				assetID = a.ID
				seen[a.ID] = true
				break
			}
		}
		if assetID == "" {
			t.Fatalf("no new asset found after scanning %s", relPath)
		}
		runID, _, err := repo.CreateModelRun(ctx, assetID, "vision", "fixture", "fixture-model", "hash-"+assetID, "facet-prompt-v1", "asset-analysis/v1", "{}")
		if err != nil {
			t.Fatal(err)
		}
		analysis := domain.StructuredAnalysis{AssetType: fx.assetType, ShotSize: fx.shotSize, CameraMotion: fx.cameraMotion, Summary: "handler facet fixture " + fx.wantID}
		// CommitAnalysisWithShots requires the run to have reached 'validated'
		// first (an active guard introduced with the model_runs boundary work);
		// the production pipeline always stages before committing, so the
		// fixture must mirror that or every commit is rejected.
		if err := repo.StageModelRun(ctx, runID, "{}", "{}"); err != nil {
			t.Fatal(err)
		}
		if err := repo.CommitAnalysisWithShots(ctx, assetID, runID, "asset-analysis/v1", analysis, nil); err != nil {
			t.Fatal(err)
		}
		// The pipeline rebuilds the FTS index as its own step after committing
		// analysis (Pipeline.execute's analyze case), so this fixture must do
		// the same or /api/v1/search would find nothing here regardless of the
		// facet under test.
		if err := repo.RebuildSearch(ctx, assetID); err != nil {
			t.Fatal(err)
		}
	}
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestListAssetCardsHandlerRejectsInvalidFacetValue(t *testing.T) {
	server := NewServer("", newFacetTestService(t))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/assets?shot_size=extremely_wide", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "shot_size") {
		t.Fatalf("body=%q, want it to name the shot_size field", response.Body.String())
	}
}

func TestListAssetCardsHandlerAppliesFacetFilter(t *testing.T) {
	server := NewServer("", newFacetTestService(t))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/assets?shot_size=wide", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var cards []domain.AssetCard
	if err := json.NewDecoder(response.Body).Decode(&cards); err != nil {
		t.Fatal(err)
	}
	// The two fixtures in newFacetTestService differ in asset_type as well as
	// shot_size (b_roll/wide vs. talking_to_camera/close_up), so asserting on
	// AssetType is a proxy for "this is the wide fixture" without depending on
	// the generated asset id.
	if len(cards) != 1 || cards[0].AssetType != "b_roll" {
		t.Fatalf("cards=%+v, want only the b_roll/wide fixture", cards)
	}
}

func TestSearchShotsHandlerRejectsInvalidFacetValue(t *testing.T) {
	server := NewServer("", newFacetTestService(t))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/search/shots?q=test&camera_motion=spin", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "camera_motion") {
		t.Fatalf("body=%q, want it to name the camera_motion field", response.Body.String())
	}
}
