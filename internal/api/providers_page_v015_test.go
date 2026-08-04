package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

func providerChannelTestService(t *testing.T, name string) *app.Service {
	t.Helper()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: t.TempDir(), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// A channel is only useful as a quota pool if the operator can keep adding keys
// to a channel that already works, so the page must ship both affordances and
// must keep the single-key flow as the default shape of the create form.
func TestProvidersPageOffersMultiKeyAffordancesWithoutBrowserStorage(t *testing.T) {
	response := httptest.NewRecorder()
	service := providerChannelTestService(t, "providers-multi-key-page.db")
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/providers", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, marker := range []string{
		`id="member-rows"`,
		"addMemberRow",
		"formMembers",
		"memberPatches",
		"patchMembers",
		"duplicateLabel",
		"addChannelKey",
		"removeChannelKey",
		`data-act="toggle-add"`,
		`data-act="save-key"`,
		`data-act="remove-member"`,
		"method:'PATCH'",
		`id="api-key"`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("providers page missing %q", marker)
		}
	}
	// The key never becomes part of a URL, never survives the page, and never
	// comes back into the DOM after a write.
	if strings.Contains(body, "localStorage") || strings.Contains(body, "sessionStorage") {
		t.Fatalf("providers page must not persist credentials in browser storage")
	}
	if !strings.Contains(body, "clearFormKeys") || !strings.Contains(body, ".a-key').value=''") {
		t.Fatalf("providers page must clear every key input after a successful write")
	}
	if strings.Contains(body, "api_key=") || strings.Contains(body, "?key=") {
		t.Fatalf("providers page must never put a key in a URL")
	}
	if strings.Contains(body, "console.log") {
		t.Fatalf("providers page must not log around key handling")
	}
}

// The page adds a key by resending the surviving members without api_key,
// because PATCH replaces the whole member set and retains a member's stored
// secret only when the member is present and its key is omitted. This test
// pins that server behaviour: it is the contract the page depends on.
func TestProviderChannelPatchAddsKeyWithoutWipingStoredSecrets(t *testing.T) {
	service := providerChannelTestService(t, "provider-channel-add-key.db")
	handler := NewServer("", service).Handler()
	token := "Bearer " + service.AdminToken()

	create := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/provider-channels", strings.NewReader(`{"capability":"video_analysis","label":"Volc plans","provider_name":"volcengine_video","endpoint":"https://example.invalid","model":"doubao","enabled":true,"members":[{"label":"plan-a","api_key":"first-plan-key","enabled":true,"weight":1,"max_inflight":1}]}`))
	request.Header.Set("Authorization", token)
	handler.ServeHTTP(create, request)
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var channel domain.ProviderChannel
	if err := json.Unmarshal(create.Body.Bytes(), &channel); err != nil {
		t.Fatal(err)
	}
	if len(channel.Members) != 1 || channel.Members[0].ID == "" {
		t.Fatalf("create returned members=%+v", channel.Members)
	}
	first := channel.Members[0]

	patch := httptest.NewRecorder()
	body := `{"members":[{"id":"` + first.ID + `","label":"plan-a","enabled":true,"weight":1,"max_inflight":1},{"label":"plan-b","api_key":"second-plan-key","enabled":true,"weight":2,"max_inflight":3}]}`
	request = httptest.NewRequest(http.MethodPatch, "/api/v1/admin/provider-channels/"+channel.ID, strings.NewReader(body))
	request.Header.Set("Authorization", token)
	handler.ServeHTTP(patch, request)
	if patch.Code != http.StatusOK {
		t.Fatalf("patch status=%d body=%s", patch.Code, patch.Body.String())
	}
	if strings.Contains(patch.Body.String(), "plan-key") || strings.Contains(patch.Body.String(), "secret_ref") {
		t.Fatalf("patch response leaked secret material: %s", patch.Body.String())
	}
	var expanded domain.ProviderChannel
	if err := json.Unmarshal(patch.Body.Bytes(), &expanded); err != nil {
		t.Fatal(err)
	}
	if len(expanded.Members) != 2 {
		t.Fatalf("expected two members, got %+v", expanded.Members)
	}
	for _, member := range expanded.Members {
		if !member.SecretReady {
			t.Fatalf("member %q lost its stored key on add: %+v", member.Label, expanded.Members)
		}
	}
	if expanded.Members[0].ID != first.ID {
		t.Fatalf("existing member id changed: %q -> %q", first.ID, expanded.Members[0].ID)
	}

	// Removing a key is the same call with the member left out.
	var second domain.ProviderChannelMember
	for _, member := range expanded.Members {
		if member.ID != first.ID {
			second = member
		}
	}
	remove := httptest.NewRecorder()
	body = `{"members":[{"id":"` + second.ID + `","label":"` + second.Label + `","enabled":true,"weight":2,"max_inflight":3}]}`
	request = httptest.NewRequest(http.MethodPatch, "/api/v1/admin/provider-channels/"+channel.ID, strings.NewReader(body))
	request.Header.Set("Authorization", token)
	handler.ServeHTTP(remove, request)
	if remove.Code != http.StatusOK {
		t.Fatalf("remove status=%d body=%s", remove.Code, remove.Body.String())
	}
	var shrunk domain.ProviderChannel
	if err := json.Unmarshal(remove.Body.Bytes(), &shrunk); err != nil {
		t.Fatal(err)
	}
	if len(shrunk.Members) != 1 || shrunk.Members[0].ID != second.ID || !shrunk.Members[0].SecretReady {
		t.Fatalf("remove left members=%+v", shrunk.Members)
	}
}
