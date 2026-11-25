//go:build playwrightfixture

package browserfixture

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFixtureCloseRemovesTemporaryDirectory(t *testing.T) {
	f, err := New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dir := f.Dir
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("fixture directory still exists after Close: %v", err)
	}
}

// TestFixtureSeedSupportsBrowserWorkflows pins the seeded shapes the browser
// scenarios drive: 301 discovered cards for the browse/saved-view page
// loaders, exactly 301 of them in the saved view, and one asset whose word
// track exceeds the timeline's 1000-row page so the drawer shows the partial
// state before Load more.
func TestFixtureSeedSupportsBrowserWorkflows(t *testing.T) {
	f, err := New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx := context.Background()
	db := f.Repo.DB()

	var cards int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM assets WHERE id LIKE 'asset-card-%'`).Scan(&cards); err != nil {
		t.Fatal(err)
	}
	if cards != 301 {
		t.Fatalf("seeded browse cards=%d, want 301", cards)
	}
	// The saved view is a persisted filter, not a card list: query its cards
	// the way the API does so the 301-row page-2 claim is checked against the
	// real projection.
	var viewID string
	if err := db.QueryRowContext(ctx, `SELECT id FROM asset_collections WHERE id='view-discovered'`).Scan(&viewID); err != nil {
		t.Fatal(err)
	}
	pageOne, err := f.Repo.ListAssetCardsInCollection(ctx, viewID, 300, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pageOne) != 300 {
		t.Fatalf("saved view first page=%d, want 300", len(pageOne))
	}
	pageTwo, err := f.Repo.ListAssetCardsInCollection(ctx, viewID, 300, 300)
	if err != nil {
		t.Fatal(err)
	}
	if len(pageTwo) != 1 || pageTwo[0].ID != "asset-card-0301" {
		t.Fatalf("saved view second page=%v, want exactly asset-card-0301", pageTwo)
	}
	for _, card := range pageOne {
		if card.ID == "asset-card-0301" {
			t.Fatal("asset-card-0301 leaked onto the saved view's first page")
		}
	}

	var words int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM transcript_words WHERE asset_id='asset-timeline-long'`).Scan(&words); err != nil {
		t.Fatal(err)
	}
	if words != 1001 {
		t.Fatalf("seeded timeline words=%d, want 1001", words)
	}
	var durationMS int
	if err := db.QueryRowContext(ctx, `SELECT json_extract(normalized_json,'$.duration_ms') FROM media_metadata WHERE asset_id='asset-timeline-long'`).Scan(&durationMS); err != nil {
		t.Fatal(err)
	}
	if durationMS <= 0 {
		t.Fatalf("long-timeline asset duration=%d, want positive", durationMS)
	}
}

// TestFixtureEncodesUploadedReference drives the pending-to-ready search the
// browser suite exercises: an administrator uploads a reference, the page's
// search reports pending, the fixture's encoder loop commits the vector, and
// the same search then answers with results.
func TestFixtureEncodesUploadedReference(t *testing.T) {
	f, err := New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	const probePng = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	raw, err := base64.StdEncoding.DecodeString(probePng)
	if err != nil {
		t.Fatal(err)
	}
	upload := httptest.NewRequest(http.MethodPost, "/api/v1/admin/search-references", bytes.NewReader(raw))
	upload.Header.Set("Authorization", "Bearer "+AdminToken)
	upload.Header.Set("Content-Type", "image/png")
	recorder := httptest.NewRecorder()
	f.Handler.ServeHTTP(recorder, upload)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var reference struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &reference); err != nil {
		t.Fatal(err)
	}

	search := func() (int, string) {
		body := fmt.Sprintf(`{"query":"","modalities":["visual"],"reference_id":%q}`, reference.ID)
		request := httptest.NewRequest(http.MethodPost, "/api/v1/search/shots", strings.NewReader(body))
		// The library's read routes trust loopback peers only; httptest's
		// synthetic default peer is a TEST-NET address.
		request.RemoteAddr = "127.0.0.1:43210"
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		f.Handler.ServeHTTP(response, request)
		return response.Code, response.Body.String()
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		status, body := search()
		if status == http.StatusOK {
			var payload struct {
				Results []json.RawMessage `json:"results"`
			}
			if err := json.Unmarshal([]byte(body), &payload); err != nil {
				t.Fatalf("ready search body=%s: %v", body, err)
			}
			if payload.Results == nil {
				t.Fatalf("ready search has no results array: %s", body)
			}
			break
		}
		if status != http.StatusAccepted || !strings.Contains(body, `"pending"`) {
			t.Fatalf("unexpected search status=%d body=%s", status, body)
		}
		if time.Now().After(deadline) {
			t.Fatal("reference never became ready; encoder loop did not commit the query vector")
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func TestServePlaywrightFixture(t *testing.T) {
	if os.Getenv("NEXUSGATE_PLAYWRIGHT_SERVE") != "1" {
		t.Skip("fixture server is opt-in")
	}
	f, err := New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	addr := os.Getenv("NEXUSGATE_PLAYWRIGHT_ADDR")
	if addr == "" {
		addr = "127.0.0.1:4173"
	}
	t.Logf("fixture server listening on https://%s", addr)
	server := &http.Server{Addr: addr, Handler: f.Handler}
	if err := server.ListenAndServeTLS(f.TLSCertificate, f.TLSKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
}
