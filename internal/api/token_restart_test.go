package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/app"
	"github.com/evjohn-icu/nexusslate/internal/config"
	"github.com/evjohn-icu/nexusslate/internal/media"
	"github.com/evjohn-icu/nexusslate/internal/repository/sqlite"
)

func TestAdminTokenSurvivesServiceReopen(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "nexusslate.db")
	open := func() (*app.Service, *sqlite.Repository) {
		repo, err := sqlite.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.Migrate(ctx); err != nil {
			repo.Close()
			t.Fatal(err)
		}
		service, err := app.NewService(repo, config.Config{DataDir: dataDir, Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
		if err != nil {
			repo.Close()
			t.Fatal(err)
		}
		return service, repo
	}
	first, repo := open()
	token := first.AdminToken()
	if token == "" {
		t.Fatal("admin token is empty")
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	second, repo := open()
	defer repo.Close()
	if second.AdminToken() != token {
		t.Fatal("admin token changed after reopen")
	}
	handler := NewServer("", second).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/roots", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code == http.StatusUnauthorized {
		t.Fatalf("reopened bearer rejected: %s", response.Body.String())
	}

	wrong := httptest.NewRequest(http.MethodGet, "/api/v1/roots", nil)
	wrong.Header.Set("Authorization", "Bearer wrong-token")
	bad := httptest.NewRecorder()
	handler.ServeHTTP(bad, wrong)
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status=%d", bad.Code)
	}
	digest := sha256.Sum256([]byte(token))
	digestRequest := httptest.NewRequest(http.MethodGet, "/api/v1/roots", nil)
	digestRequest.Header.Set("Authorization", "Bearer "+hex.EncodeToString(digest[:]))
	digestResponse := httptest.NewRecorder()
	handler.ServeHTTP(digestResponse, digestRequest)
	if digestResponse.Code != http.StatusUnauthorized {
		t.Fatalf("digest accepted with status=%d", digestResponse.Code)
	}
}
