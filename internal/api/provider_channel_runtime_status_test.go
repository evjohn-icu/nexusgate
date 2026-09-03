package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/app"
	"github.com/evjohn-icu/nexusslate/internal/providerchannels"
)

// TestProviderChannelRuntimeStatusDistinguishesRetiredFromLiveMember is the
// discriminating check for GET /api/v1/admin/provider-channels/status. It
// drives the real Handler() as an authenticated Hub admin, and it gets a
// member genuinely retired the only way that happens outside a unit test
// setting the field directly: by making the pool observe a 401 from a real
// (fixture) HTTP round trip.
//
// The setup is one tag_curator channel with two members that share an
// endpoint and differ only by API key -- "bad", which the fixture server
// answers with 401 no matter what it's asked, and "good", which it answers
// with a valid completion. POST /api/v1/tags/curate runs
// Service.RunTagCurator, which calls the real channelTagCurator, which goes
// through providerchannels.Executor.Execute exactly as production traffic
// would: Select finds "bad" or "good" in either order, and whichever member
// answers 401 is classified providerpool.MemberSpent and retired by the pool
// before Execute moves on to the other. Both possible orders leave the run
// succeeding (the survivor answers 200) and leave exactly one member
// retired, so the assertions below don't depend on which member the pool
// happened to pick first.
func TestProviderChannelRuntimeStatusDistinguishesRetiredFromLiveMember(t *testing.T) {
	const badKey = "bad-secret-key"
	const goodKey = "good-secret-key"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch auth {
		case "Bearer " + badKey:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
		case "Bearer " + goodKey:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"groups\":[]}"}}]}`))
		default:
			t.Fatalf("unexpected Authorization header %q", auth)
		}
	}))
	defer server.Close()

	service := providerChannelTestService(t, "provider-channel-runtime-status.db")
	handler := NewServer("", service).Handler()
	token := "Bearer " + service.AdminToken()

	createBody := `{"capability":"tag_curator","label":"Curator pool","provider_name":"openai_chat","endpoint":"` + server.URL + `","model":"curator-test","enabled":true,"members":[` +
		`{"label":"bad","api_key":"` + badKey + `","enabled":true,"weight":1,"max_inflight":1},` +
		`{"label":"good","api_key":"` + goodKey + `","enabled":true,"weight":1,"max_inflight":1}` +
		`]}`
	create := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/provider-channels", strings.NewReader(createBody))
	req.Header.Set("Authorization", token)
	handler.ServeHTTP(create, req)
	if create.Code != http.StatusCreated {
		t.Fatalf("create channel status=%d body=%s", create.Code, create.Body.String())
	}

	// Before anything has routed through tag_curator, the capability must
	// report no runtime data -- this is the "freshly restarted Hub" case the
	// brief calls out, and it must not be silently reported as healthy.
	before := fetchProviderChannelRuntimeStatus(t, handler, token)
	beforeCurator := findCapabilityStatus(t, before, providerchannels.CapabilityTagCurator)
	if beforeCurator.HasRuntimeData {
		t.Fatalf("tag_curator reported runtime data before any request routed through it: %+v", beforeCurator)
	}
	if beforeCurator.Snapshot != nil {
		t.Fatalf("tag_curator carried a snapshot before any request routed through it: %+v", beforeCurator.Snapshot)
	}

	// Drive the retirement for real: run the tag curator through the actual
	// HTTP handler, which calls Service.RunTagCurator -> the real
	// channelTagCurator -> providerchannels.Executor.Execute -> the fixture
	// server above. This is what makes one member's 401 a genuine pool
	// decision rather than a fabricated field.
	curate := httptest.NewRecorder()
	curateReq := httptest.NewRequest(http.MethodPost, "/api/v1/tags/curate?limit=1", nil)
	curateReq.Header.Set("Authorization", token)
	handler.ServeHTTP(curate, curateReq)
	if curate.Code != http.StatusCreated {
		t.Fatalf("run tag curator status=%d body=%s", curate.Code, curate.Body.String())
	}

	after := fetchProviderChannelRuntimeStatus(t, handler, token)
	afterCurator := findCapabilityStatus(t, after, providerchannels.CapabilityTagCurator)
	if !afterCurator.HasRuntimeData || afterCurator.Snapshot == nil {
		t.Fatalf("tag_curator reported no runtime data after a request routed through it: %+v", afterCurator)
	}
	if len(afterCurator.Snapshot.Channels) != 1 {
		t.Fatalf("expected one channel in snapshot, got %+v", afterCurator.Snapshot.Channels)
	}
	members := afterCurator.Snapshot.Channels[0].Members
	if len(members) != 2 {
		t.Fatalf("expected two members in snapshot, got %+v", members)
	}
	var retiredCount, liveCount int
	for _, member := range members {
		switch member.Label {
		case "bad":
			if !member.Retired {
				t.Fatalf("member %q answered 401 but was not reported retired: %+v", member.Label, member)
			}
			if member.LastFailureRetryable {
				t.Fatalf("member %q's retirement was reported retryable, which would tell an operator to just wait: %+v", member.Label, member)
			}
			retiredCount++
		case "good":
			if member.Retired {
				t.Fatalf("member %q succeeded but was reported retired: %+v", member.Label, member)
			}
			if member.Attempts == 0 {
				t.Fatalf("member %q was never actually attempted: %+v", member.Label, member)
			}
			liveCount++
		default:
			t.Fatalf("unexpected member label %q", member.Label)
		}
	}
	if retiredCount != 1 || liveCount != 1 {
		t.Fatalf("want exactly one retired and one live member, got retired=%d live=%d members=%+v", retiredCount, liveCount, members)
	}

	// 1: no secret material anywhere in the response. Enumerate what is
	// checked rather than trusting the type -- the same discipline the
	// brief asked for on MemberStatus/ChannelStatus themselves.
	raw, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, secret := range []string{badKey, goodKey, "Bearer " + badKey, "Bearer " + goodKey} {
		if strings.Contains(body, secret) {
			t.Fatalf("runtime status response leaked a provider key: contains %q\nbody=%s", secret, body)
		}
	}
	for _, field := range []string{`"secret_ref"`, `"SecretRef"`, `"api_key"`, `"APIKey"`} {
		if strings.Contains(body, field) {
			t.Fatalf("runtime status response carried a secret-shaped field %q\nbody=%s", field, body)
		}
	}
}

func fetchProviderChannelRuntimeStatus(t *testing.T, handler http.Handler, token string) []app.ProviderChannelCapabilityStatus {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/provider-channels/status", nil)
	req.Header.Set("Authorization", token)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("runtime status status=%d body=%s", rec.Code, rec.Body.String())
	}
	var statuses []app.ProviderChannelCapabilityStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &statuses); err != nil {
		t.Fatalf("decode runtime status: %v\nbody=%s", err, rec.Body.String())
	}
	return statuses
}

func findCapabilityStatus(t *testing.T, statuses []app.ProviderChannelCapabilityStatus, capability providerchannels.Capability) app.ProviderChannelCapabilityStatus {
	t.Helper()
	for _, status := range statuses {
		if status.Capability == capability {
			return status
		}
	}
	t.Fatalf("no status entry for capability %q in %+v", capability, statuses)
	return app.ProviderChannelCapabilityStatus{}
}

// TestProviderChannelRuntimeStatusRequiresHubAdmin pins that the new route
// follows the same admin gate as every other /api/v1/admin/provider-channels
// route (server.go:104-110) rather than accidentally landing on a public or
// worker-scoped path.
func TestProviderChannelRuntimeStatusRequiresHubAdmin(t *testing.T) {
	service := providerChannelTestService(t, "provider-channel-runtime-status-auth.db")
	handler := NewServer("", service).Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/provider-channels/status", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated status request status=%d body=%s; want 401/403", rec.Code, rec.Body.String())
	}
}
