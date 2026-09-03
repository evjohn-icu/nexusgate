package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/app"
	"github.com/evjohn-icu/nexusslate/internal/config"
	"github.com/evjohn-icu/nexusslate/internal/domain"
	"github.com/evjohn-icu/nexusslate/internal/media"
	"github.com/evjohn-icu/nexusslate/internal/repository/sqlite"
)

// TestProviderChannelPatchDuplicateLabelRejected400 pins the operator-facing
// contract for the update path: a patch that would produce two same-label
// members is a 400-class client error, never a 500, and the response never
// echoes the submitted key material.
func TestProviderChannelPatchDuplicateLabelRejected400(t *testing.T) {
	service := providerChannelTestService(t, "provider-channel-dup-label.db")
	handler := NewServer("", service).Handler()
	token := "Bearer " + service.AdminToken()

	create := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/provider-channels", strings.NewReader(`{"capability":"video_analysis","label":"Dup channel","provider_name":"gemini","endpoint":"https://example.invalid","model":"gemini-flash","enabled":true,"members":[{"label":"primary","api_key":"first-key","enabled":true,"weight":1,"max_inflight":1}]}`))
	req.Header.Set("Authorization", token)
	handler.ServeHTTP(create, req)
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var channel domain.ProviderChannel
	if err := json.Unmarshal(create.Body.Bytes(), &channel); err != nil {
		t.Fatal(err)
	}

	patch := httptest.NewRecorder()
	body := `{"members":[{"label":"dupe","api_key":"key-a","enabled":true,"weight":1,"max_inflight":1},{"label":"dupe","api_key":"key-b","enabled":true,"weight":1,"max_inflight":1}]}`
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/admin/provider-channels/"+channel.ID, strings.NewReader(body))
	req.Header.Set("Authorization", token)
	handler.ServeHTTP(patch, req)
	if patch.Code < 400 || patch.Code >= 500 {
		t.Fatalf("duplicate-label patch status=%d; want 400-class, body=%s", patch.Code, patch.Body.String())
	}
	if strings.Contains(patch.Body.String(), "key-a") || strings.Contains(patch.Body.String(), "key-b") {
		t.Fatalf("duplicate-label rejection leaked key material: %s", patch.Body.String())
	}
	if !strings.Contains(patch.Body.String(), "dupe") {
		t.Fatalf("duplicate-label patch response should name the label, got: %s", patch.Body.String())
	}
}

// TestProviderChannelCreateDuplicateLabelRejected400 is the create-path
// counterpart: before this fix, a duplicate label on create was never
// validated in internal/app and reached SQLite, so the response body was a
// bare "UNIQUE constraint failed: provider_channel_members.channel_id,
// provider_channel_members.label (2067)" storage error. The assertion that
// matters is not just the status code — it is that none of that storage
// vocabulary is reachable from the response body, and that the label the
// operator typed twice is.
func TestProviderChannelCreateDuplicateLabelRejected400(t *testing.T) {
	service := providerChannelTestService(t, "provider-channel-create-dup-label.db")
	handler := NewServer("", service).Handler()
	token := "Bearer " + service.AdminToken()

	body := `{"capability":"video_analysis","label":"Dup create channel","provider_name":"gemini","endpoint":"https://example.invalid","model":"gemini-flash","enabled":true,"members":[{"label":"dupe","api_key":"key-a","enabled":true,"weight":1,"max_inflight":1},{"label":"dupe","api_key":"key-b","enabled":true,"weight":1,"max_inflight":1}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/provider-channels", strings.NewReader(body))
	req.Header.Set("Authorization", token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("duplicate-label create status=%d; want 400-class, body=%s", rec.Code, rec.Body.String())
	}
	responseBody := rec.Body.String()
	if strings.Contains(responseBody, "key-a") || strings.Contains(responseBody, "key-b") {
		t.Fatalf("duplicate-label create response leaked key material: %s", responseBody)
	}
	if !strings.Contains(responseBody, "dupe") {
		t.Fatalf("duplicate-label create response should name the label, got: %s", responseBody)
	}
	for _, leaked := range []string{"UNIQUE", "constraint", "2067", "provider_channel_members"} {
		if strings.Contains(responseBody, leaked) {
			t.Fatalf("duplicate-label create response leaked storage-layer detail %q: %s", leaked, responseBody)
		}
	}
}

// TestProviderChannelCreateNonValidationFailureNotEchoed is the discriminating
// test for N2: SaveProviderChannel also calls UpsertProviderChannel and the
// secret store, and the create handler used to echo err.Error() verbatim for
// every failure, not just a validation one. This forces the repository layer
// to fail for a reason that has nothing to do with operator input and asserts
// that text never reaches the client — without this, "only echo a validation
// error" is indistinguishable from "rename the message".
func TestProviderChannelCreateNonValidationFailureNotEchoed(t *testing.T) {
	const sentinel = "internal-detail-must-not-escape"
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "provider-channel-create-repo-fail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	stub := &upsertFailingRepo{Repository: repo, err: errors.New(sentinel)}
	service, err := app.NewService(stub, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()
	token := "Bearer " + service.AdminToken()

	body := `{"capability":"video_analysis","label":"Repo fail channel","provider_name":"gemini","endpoint":"https://example.invalid","model":"gemini-flash","enabled":true,"members":[{"label":"primary","api_key":"first-key","enabled":true,"weight":1,"max_inflight":1}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/provider-channels", strings.NewReader(body))
	req.Header.Set("Authorization", token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("non-validation create failure status=%d; want 400-class, body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), sentinel) {
		t.Fatalf("non-validation failure text reached the response body: %s", rec.Body.String())
	}
}

// upsertFailingRepo wraps a real sqlite repository and overrides only
// UpsertProviderChannel, so every other call (secret store setup inside
// NewService, etc.) behaves exactly like the real Hub and only the final
// write fails. That isolates the failure to "the repository rejected this
// for a reason that is not the operator's input" without needing a
// hand-rolled fake for the rest of app.Repository's surface.
type upsertFailingRepo struct {
	*sqlite.Repository
	err error
}

func (r *upsertFailingRepo) UpsertProviderChannel(_ context.Context, _ domain.ProviderChannel) (domain.ProviderChannel, error) {
	return domain.ProviderChannel{}, r.err
}
