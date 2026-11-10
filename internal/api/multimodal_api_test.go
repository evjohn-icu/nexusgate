package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func doJSON(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.RemoteAddr = "10.0.0.5:1234"
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestMultimodalSettingsDefaultToDisabled pins the upgrade behaviour: an
// install that never opted in must not start running models, and the API says
// so instead of implying a capability it does not have.
func TestMultimodalSettingsDefaultToDisabled(t *testing.T) {
	_, handler := newAdminAuthModeServer(t, "off")
	recorder := doJSON(t, handler, http.MethodGet, "/api/v1/admin/multimodal/settings", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Settings struct {
			Enabled map[string]bool `json:"enabled"`
		} `json:"settings"`
		LocalRuntime struct {
			Available bool `json:"available"`
		} `json:"local_runtime"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"visual_embedding", "sound_events", "audio_embedding", "diarization", "speaker_embedding"} {
		if response.Settings.Enabled[operation] {
			t.Errorf("operation %q must default to disabled", operation)
		}
	}
	if response.LocalRuntime.Available {
		t.Error("a service without an executor must report the local runtime unavailable")
	}
}

func TestMultimodalSettingsRejectsUnknownOperationAndModel(t *testing.T) {
	_, handler := newAdminAuthModeServer(t, "off")
	recorder := doJSON(t, handler, http.MethodPut, "/api/v1/admin/multimodal/settings",
		`{"enabled":{"teleport":true}}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown operation: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = doJSON(t, handler, http.MethodPut, "/api/v1/admin/multimodal/settings",
		`{"enabled":{"visual_embedding":true},"profiles":{"visual_embedding":"not-a-model"}}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown model: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = doJSON(t, handler, http.MethodPut, "/api/v1/admin/multimodal/settings",
		`{"enabled":{"sound_events":true},"profiles":{"sound_events":"visual-siglip2-base"}}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("model that cannot execute the operation must be rejected: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestMultimodalSettingsRoundTrip(t *testing.T) {
	_, handler := newAdminAuthModeServer(t, "off")
	recorder := doJSON(t, handler, http.MethodPut, "/api/v1/admin/multimodal/settings",
		`{"enabled":{"visual_embedding":true,"sound_events":true},"profiles":{"visual_embedding":"visual-siglip2-base"}}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = doJSON(t, handler, http.MethodGet, "/api/v1/admin/multimodal/settings", "")
	var response struct {
		Settings struct {
			Enabled  map[string]bool   `json:"enabled"`
			Profiles map[string]string `json:"profiles"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Settings.Enabled["visual_embedding"] || !response.Settings.Enabled["sound_events"] {
		t.Fatalf("enabled operations were not persisted: %+v", response.Settings.Enabled)
	}
	if response.Settings.Enabled["diarization"] {
		t.Fatal("an untouched operation must stay disabled")
	}
	if response.Settings.Profiles["visual_embedding"] != "visual-siglip2-base" {
		t.Fatalf("profile selection was not persisted: %+v", response.Settings.Profiles)
	}
}

// TestSpeakerConsentIsDefaultOffAndGatesProfiles pins the privacy contract: no
// extraction and no profile without an explicit opt-in, and the generation only
// moves when the value actually changes.
func TestSpeakerConsentIsDefaultOffAndGatesProfiles(t *testing.T) {
	_, handler := newAdminAuthModeServer(t, "off")
	recorder := doJSON(t, handler, http.MethodGet, "/api/v1/admin/speaker-consent", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var consent struct {
		Enabled    bool  `json:"enabled"`
		Generation int64 `json:"generation"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &consent); err != nil {
		t.Fatal(err)
	}
	if consent.Enabled {
		t.Fatal("speaker consent must default to disabled")
	}

	recorder = doJSON(t, handler, http.MethodPost, "/api/v1/admin/speaker-profiles",
		`{"asset_id":"a","diarization_generation_id":"g","local_speaker_id":"SPEAKER_00"}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("profile creation without consent: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = doJSON(t, handler, http.MethodPut, "/api/v1/admin/speaker-consent", `{"enabled":true}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("enable consent: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &consent); err != nil {
		t.Fatal(err)
	}
	if !consent.Enabled || consent.Generation != 1 {
		t.Fatalf("unexpected consent after enable: %+v", consent)
	}
	// Repeating the same value must not bump the generation: every in-flight
	// extraction captured the old value and would be rejected for nothing.
	recorder = doJSON(t, handler, http.MethodPut, "/api/v1/admin/speaker-consent", `{"enabled":true}`)
	if err := json.Unmarshal(recorder.Body.Bytes(), &consent); err != nil {
		t.Fatal(err)
	}
	if consent.Generation != 1 {
		t.Fatalf("an unchanged consent value bumped the generation to %d", consent.Generation)
	}
	recorder = doJSON(t, handler, http.MethodPut, "/api/v1/admin/speaker-consent", `{"enabled":false}`)
	if err := json.Unmarshal(recorder.Body.Bytes(), &consent); err != nil {
		t.Fatal(err)
	}
	if consent.Enabled || consent.Generation != 2 {
		t.Fatalf("unexpected consent after disable: %+v", consent)
	}
}

func TestSpeakerAssociationPatchRejectsUnknownTransition(t *testing.T) {
	_, handler := newAdminAuthModeServer(t, "off")
	doJSON(t, handler, http.MethodPut, "/api/v1/admin/speaker-consent", `{"enabled":true}`)
	recorder := doJSON(t, handler, http.MethodPatch, "/api/v1/admin/speaker-associations/missing",
		`{"state":"possible_match"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("an operator must not be able to set a candidate state: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
