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
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
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

// 权重 and 并发上限 are defaults for all but the operator tuning them, so both
// the create form and the add-key template hide them behind a collapsed 高级配置
// disclosure. The ids and classes the page JS reads (formMembers and
// addChannelKey query .m-*/row .a-* by class) must survive the wrapper.
func TestProvidersPageAdvancedConfigKeepsFieldIdsAndClasses(t *testing.T) {
	response := httptest.NewRecorder()
	service := providerChannelTestService(t, "providers-advanced-config-page.db")
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/providers", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, marker := range []string{
		`<details class="advanced-config wide">`,
		`<summary>高级配置</summary>`,
		`id="weight"`,
		`id="max-inflight"`,
		`class="m-weight"`,
		`class="m-inflight"`,
		`+prefix+'weight"`,
		`+prefix+'inflight"`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("providers page missing %q", marker)
		}
	}
	if !strings.Contains(body, `row.querySelector('.m-weight')`) || !strings.Contains(body, `row.querySelector('.m-inflight')`) {
		t.Fatalf("formMembers() must keep reading weight/max_inflight from the member rows")
	}
	if !strings.Contains(body, `form.querySelector('.a-weight')`) || !strings.Contains(body, `form.querySelector('.a-inflight')`) {
		t.Fatalf("addChannelKey() must keep reading weight/max_inflight from the add form")
	}
}

// The channels panel carries one runtime-health line fed by
// /api/v1/admin/provider-channels/status: nothing without a token, a muted
// 暂无运行数据 when no capability has been exercised yet, a warn pill when any
// route's channel is unavailable, and an ok pill otherwise. It must render the
// states verbatim so the pill semantics cannot drift from the endpoint.
func TestProvidersPageShowsRuntimeHealthStatusLine(t *testing.T) {
	body := providersHTML
	for _, marker := range []string{
		`id="provider-health"`,
		`/api/v1/admin/provider-channels/status`,
		`has_runtime_data`,
		`ch.available`,
		`暂无运行数据`,
		`部分服务降级`,
		`服务正常`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("providers page missing health marker %q", marker)
		}
	}
	if !strings.Contains(body, `if(!r.ok){el.innerHTML='';return}`) {
		t.Fatalf("provider health must render nothing when the status request is not authorized")
	}
}

// The same status fetch that feeds the top health line also feeds per-member
// state: loadProviderHealth stashes the parsed response (statusCache) plus a
// channel-id → member lookup (channelHealth) and the render function builds
// each member row's state pill and counter line from it. The pill states and
// the meta-line labels must stay verbatim so the UI cannot drift from the
// G1a MemberStatus fields.
func TestProvidersPageRendersPerMemberHealth(t *testing.T) {
	body := providersHTML
	for _, marker := range []string{
		`statusCache=[]`,
		`channelHealth=new Map()`,
		`anyRuntimeData`,
		`memberHealthRow(c,m)`,
		`memberHealth(s)`,
		`healthMeta(s)`,
		`last_success_at`,
		`cooldown_until`,
		`half_open`,
		`last_failure_retryable`,
		`<span class="pill bad">退休</span>`,
		`<span class="pill warn">冷却中`,
		`<span class="pill ok">正常</span>`,
		`<span class="pill warn">最近失败（可重试）</span>`,
		`<span class="pill bad">最近失败</span>`,
		`成功 '+(Number(s.successes)||0)`,
		`429 '+(Number(s.retryable_429)||0)`,
		`5xx '+(Number(s.server_error_5xx)||0)`,
		`延迟 '+(Number(s.latency_ms)||0)`,
		`上次成功`,
		`无记录`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("providers page missing per-member health marker %q", marker)
		}
	}
	// Fresh restart: no capability has runtime data yet, so member rows render
	// the muted line instead of any pill, and the health line agrees.
	if !strings.Contains(body, `if(!anyRuntimeData)return'<div class="member-health muted">暂无运行数据</div>'`) {
		t.Fatalf("member rows must show 暂无运行数据 when the status fetch has no runtime data")
	}
	// The health line and the member rows must not fight over one fetch: the
	// status request is awaited before renderChannels so the first paint
	// already has the member lookup populated.
	if !strings.Contains(body, `await loadProviderHealth()`) {
		t.Fatalf("loadChannels must await the status fetch before rendering member rows")
	}
}

// Each channel card carries a 测试 button that POSTs
// /api/v1/admin/provider-channels/{id}/test with the admin token and renders
// the extended result inline under a channel-scoped id. The result span must
// be per-channel (test-result-<channelId>) so two cards never fight over one
// element, and the call must go through adminHeaders() so the in-memory token
// is what authenticates it.
func TestProvidersPageHasTestChannelButton(t *testing.T) {
	body := providersHTML
	for _, marker := range []string{
		`data-act="test-channel"`,
		`测试</button>`,
		`test-result-'+esc(c.id)`,
		`model_responded`,
		`模型已响应`,
		`encodeURIComponent(channel.id)+'/test'`,
		"adminHeaders()",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("providers page missing test marker %q", marker)
		}
	}
}

// The channel dialog must let an operator configure a known provider by
// pasting just the key: selecting the provider pre-fills its well-known
// endpoint, and a 读取模型 button POSTs the still-open form's endpoint and key
// to /api/v1/admin/provider-channels/probe-models so the Hub can return the
// account's model list into a datalist. The presets must never be applied over
// an operator-typed endpoint (endpointTouched), and the probe must go through
// adminHeaders() exactly like every other channel write.
func TestProvidersPageHasOneClickModelProbe(t *testing.T) {
	body := providersHTML
	for _, marker := range []string{
		`读取模型</button>`,
		`id="probe-models"`,
		`probe-models'`,
		`list="model-options"`,
		`id="model-options"`,
		`const endpointPresets={`,
		`ark.cn-beijing.volces.com/api/plan/v3`,
		`ark.cn-beijing.volces.com/api/coding/v3`,
		`dashscope.aliyuncs.com/compatible-mode/v1`,
		`function applyEndpointPreset()`,
		`endpointTouched`,
		`function probeModels()`,
		`provider_name:provider`,
		`api_key:key`,
		"adminHeaders(true)",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("providers page missing one-click probe marker %q", marker)
		}
	}
}

// The 一键配置 wizard must let an operator provision every missing capability
// from one dialog and one submit with only the plan key pasted — endpoint and
// model come pre-filled from presets, but the model field stays editable so a
// user can specify their own model. Beside the plan presets there is a custom
// endpoint mode: endpoint + key, the program probes the model list, and the
// operator ticks which roles that endpoint should serve (each role's model
// pre-filled, editable).
func TestProvidersPageHasOneClickQuickConfigWizard(t *testing.T) {
	body := providersHTML
	for _, marker := range []string{
		`id="quick-config-btn"`,
		`一键配置`,
		`id="quick-dialog"`,
		`id="quick-rows"`,
		`id="quick-go"`,
		`quickSubmit()`,
		`const planPresets=[`,
		`function quickPlanRowHTML(plan)`,
		`function quickChannelRowHTML(ch)`,
		`火山 Agent Plan`,
		`火山 Coding Plan`,
		`Qwen Token Plan`,
		`doubao-seed-2.0-lite`,
		`qwen3.7-plus`,
		`capNames`,
		`data-plan="'+plan.id+'"`,
		`class="q-model" data-cap="'+ch.cap+'"`,
		`可自定义`,
		`row.dataset.configured`,
		`已配置`,
		`customProbe()`,
		`custom-roles`,
		`q-role-model`,
		`probe-models`,
		`q-key').forEach(function(k){k.value=''`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("providers page missing quick-config marker %q", marker)
		}
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
