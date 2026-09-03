package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/app"
	"github.com/evjohn-icu/nexusslate/internal/config"
	"github.com/evjohn-icu/nexusslate/internal/domain"
	"github.com/evjohn-icu/nexusslate/internal/media"
	"github.com/evjohn-icu/nexusslate/internal/repository/sqlite"
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

// providersFragmentKeys loads the page's fragment file and returns its zh-CN
// key set. The fragment is not merged into the embedded catalogs yet, so the
// page tests assert the key wiring against the fragment on disk rather than a
// resolved zh-CN value.
func providersFragmentKeys(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("locales", "fragments", "providers.json"))
	if err != nil {
		t.Fatalf("read providers fragment: %v", err)
	}
	var frag struct {
		Page string                       `json:"page"`
		Keys map[string]map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(data, &frag); err != nil {
		t.Fatalf("parse providers fragment: %v", err)
	}
	keys := make(map[string]bool)
	for k := range frag.Keys[string(localeZhCN)] {
		keys[k] = true
	}
	return keys
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
// the create form and the add-key template hide them behind a collapsed
// [[i18n:providers.advanced]] disclosure. The ids and classes the page JS reads
// (formMembers and addChannelKey query .m-*/row .a-* by class) must survive the
// wrapper.
func TestProvidersPageAdvancedConfigKeepsFieldIdsAndClasses(t *testing.T) {
	body := providersHTML
	for _, marker := range []string{
		`<details class="advanced-config wide">`,
		`<summary>[[i18n:providers.advanced]]</summary>`,
		`id="weight"`,
		`id="max-inflight"`,
		`class="m-weight"`,
		`class="m-inflight"`,
		`+prefix+'weight"`,
		`+prefix+'inflight"`,
		`tdT('providers.advanced')`,
		`tdT('providers.weight')`,
		`tdT('providers.maxInflight')`,
		`tdT('providers.memberLabel')`,
		`tdT('providers.apiKey')`,
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
// tdT('providers.noUsage') line when no capability has been exercised yet, a
// warn pill when any route's channel is unavailable, and an ok pill otherwise.
// It must render the states through catalog keys so the pill semantics cannot
// drift from the endpoint.
func TestProvidersPageShowsRuntimeHealthStatusLine(t *testing.T) {
	body := providersHTML
	for _, marker := range []string{
		`id="provider-health"`,
		`/api/v1/admin/provider-channels/status`,
		`has_runtime_data`,
		`ch.available`,
		`tdT('providers.noUsage')`,
		`tdT('providers.degraded')`,
		`tdT('providers.healthy')`,
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
// the meta-line labels must render through catalog keys so the UI cannot drift
// from the G1a MemberStatus fields.
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
		`tdT('status.disabled')`,
		`tdT('providers.cooling')`,
		`tdT('status.healthy')`,
		`tdT('providers.recentFailureRetryable')`,
		`tdT('providers.recentFailure')`,
		`tdT('providers.metaSuccess'`,
		`tdT('providers.metaRateLimited'`,
		`tdT('providers.metaServerError'`,
		`tdT('providers.metaLatency'`,
		`tdT('providers.metaLastSuccess'`,
		`tdT('providers.noRecord')`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("providers page missing per-member health marker %q", marker)
		}
	}
	// Fresh restart: no capability has runtime data yet, so member rows render
	// the muted line instead of any pill, and the health line agrees.
	if !strings.Contains(body, `if(!anyRuntimeData)return'<div class="member-health muted">'+esc(tdT('providers.noUsage'))+'</div>'`) {
		t.Fatalf("member rows must show the no-usage line when the status fetch has no runtime data")
	}
	// The health line and the member rows must not fight over one fetch: the
	// status request is awaited before renderChannels so the first paint
	// already has the member lookup populated.
	if !strings.Contains(body, `await loadProviderHealth()`) {
		t.Fatalf("loadChannels must await the status fetch before rendering member rows")
	}
}

// Each channel card carries a test button that POSTs
// /api/v1/admin/provider-channels/{id}/test with the admin token and renders
// the extended result inline under a channel-scoped id. The result span must
// be per-channel (test-result-<channelId>) so two cards never fight over one
// element, and the call must go through adminHeaders() so the in-memory token
// is what authenticates it.
func TestProvidersPageHasTestChannelButton(t *testing.T) {
	body := providersHTML
	for _, marker := range []string{
		`data-act="test-channel"`,
		`tdT('providers.testChannel')`,
		`test-result-'+esc(c.id)`,
		`model_responded`,
		`tdT('providers.testOk'`,
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
// endpoint, and the probe button POSTs the still-open form's endpoint and key
// to /api/v1/admin/provider-channels/probe-models so the Hub can return the
// account's model list into a datalist. The presets must never be applied over
// an operator-typed endpoint (endpointTouched), and the probe must go through
// adminHeaders() exactly like every other channel write.
func TestProvidersPageHasOneClickModelProbe(t *testing.T) {
	body := providersHTML
	for _, marker := range []string{
		`[[i18n:providers.probeModels]]</button>`,
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

// The beginner wizard is the default entry point: #new-channel-btn opens
// #add-provider-dialog with a capability-first, one-channel-per-save shape,
// while the full channel editor stays reachable through #advanced-btn and the
// per-card edit action. The capability is chosen first (video_analysis or asr)
// and the five multi-capability checkboxes are gone. The key lives only in a
// password input, is cleared on success, and a 401/403 surfaces the shared
// admin prompt.
func TestProvidersPageBeginnerWizardIsDefaultEntry(t *testing.T) {
	body := providersHTML
	for _, marker := range []string{
		`id="add-provider-dialog"`,
		`[[i18n:providers.addProvider]]`,
		`[[i18n:providers.advancedRouting]]`,
		`id="wizard-capability"`,
		`id="wizard-provider"`,
		`id="wizard-endpoint"`,
		`id="wizard-key"`,
		`id="wizard-model"`,
		`id="wizard-model-options"`,
		`list="wizard-model-options"`,
		`id="wizard-detect"`,
		`id="wizard-save"`,
		`onclick="wizardDetect()"`,
		`onclick="wizardSave()"`,
		`syncWizardProviders`,
		`wizardSelectProvider`,
		`wizardDetect`,
		`wizardSave`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("providers page missing wizard marker %q", marker)
		}
	}
	// The capability is a single select chosen first; the multi-capability
	// checkboxes are gone, so one submit means one channel.
	if !strings.Contains(body, `<select id="wizard-capability"><option value="video_analysis">`) {
		t.Fatalf("wizard must choose a capability first with video_analysis and asr options")
	}
	if strings.Contains(body, `class="wizard-cap"`) {
		t.Fatalf("wizard capability checkboxes must be gone (capability is a single select)")
	}
	// #new-channel-btn must open the wizard; #advanced-btn must open the full
	// channel editor (openCreate), which is also what edit-channel keeps using.
	if !strings.Contains(body, `document.getElementById('new-channel-btn').addEventListener('click',openAddProviderDialog)`) {
		t.Fatalf("new-channel-btn must open the beginner wizard")
	}
	if !strings.Contains(body, `document.getElementById('advanced-btn').addEventListener('click',openCreate)`) {
		t.Fatalf("advanced-btn must open the full channel editor")
	}
	// The wizard key must stay in a live password input and be cleared on
	// success, never stored; a 401/403 must surface the shared admin prompt.
	if !strings.Contains(body, `id="wizard-key" type="password" autocomplete="new-password"`) {
		t.Fatalf("wizard key input must be a password field with autocomplete=new-password")
	}
	if !strings.Contains(body, `document.getElementById('wizard-key').value=''`) {
		t.Fatalf("wizard must clear the key input on success")
	}
	if !strings.Contains(body, `r.status===401||r.status===403){openAdminDialog()`) {
		t.Fatalf("wizard must surface the shared admin prompt on 401/403")
	}
}

// The wizard presets are the grounded beginner matrix: each capability lists
// exactly its key-backed providers, and each entry carries the protocol and
// default model the config defaults use. Selecting a provider must prefill
// both the endpoint preset and the editable model, so the operator only types
// the key. local_vlm is not a beginner choice (channel execution needs a
// stored secret reference); it stays in the advanced list with guidance.
func TestProvidersPageWizardGroundedPresets(t *testing.T) {
	body := providersHTML
	for _, preset := range []string{
		`['gemini','gemini_generate_content','gemini-2.5-flash']`,
		`['qwen_video','openai_video','qwen3.6-flash']`,
		`['volcengine_video','openai_video','doubao-1.5-vision-pro-250428']`,
		`['stepfun','','stepaudio-2.5-asr']`,
		`['qwen','','qwen3-asr-flash']`,
		`['volcengine_asr','','doubao-seed-asr-2.0']`,
	} {
		if !strings.Contains(body, preset) {
			t.Fatalf("wizard missing grounded preset %q", preset)
		}
	}
	// local_vlm and openai_chat must not appear as beginner presets.
	var presetLine string
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "const wizardPresets=") {
			presetLine = line
			break
		}
	}
	if presetLine == "" {
		t.Fatal("wizardPresets const not found")
	}
	if strings.Contains(presetLine, "'local_vlm'") || strings.Contains(presetLine, "'openai_chat'") {
		t.Fatalf("wizard must not offer local_vlm or openai_chat as beginner presets")
	}
	// Selecting a provider prefills the endpoint and the editable model input.
	if !strings.Contains(body, `if(el)el.value=endpointPresets[provider]||''`) {
		t.Fatalf("wizardSelectProvider must prefill the endpoint preset")
	}
	if !strings.Contains(body, `model.value=wizardModelPreset()||''`) {
		t.Fatalf("wizardSelectProvider must prefill the preset model on every provider change")
	}
	// The model control is an editable input fed by a datalist, not a select.
	if !strings.Contains(body, `<input id="wizard-model" list="wizard-model-options"`) {
		t.Fatalf("wizard model must be an editable input with a datalist")
	}
	// Volcengine ASR's config-default endpoint is part of the preset set.
	if !strings.Contains(body, `volcengine_asr:'wss://openspeech.bytedance.com/api/v3/plan/sauc/bigmodel_nostream'`) {
		t.Fatalf("endpoint presets must include the Volcengine ASR default endpoint")
	}
	// local_vlm stays in the advanced provider list (wire value only; labels
	// render through providerNameKeys), with the config-path note.
	if !strings.Contains(body, `providerOptions={video_analysis:['gemini','qwen_video','volcengine_video','local_vlm']`) {
		t.Fatalf("advanced provider list must keep local_vlm")
	}
	if !strings.Contains(body, `id="provider-local-note" hidden>[[i18n:providers.localVlmNote]]`) {
		t.Fatalf("advanced dialog must carry the local VLM config-path note")
	}
	if !strings.Contains(body, `applyProviderNote`) {
		t.Fatalf("provider note must be toggled as the advanced provider changes")
	}
}

// Model detection in the wizard is optional: a successful probe fills the
// datalist without overwriting typed input; unprobeable/no_models/schema_unknown
// keep the editable preset and explain that detection is unavailable instead of
// blocking save; only a key_invalid probe blocks saving until the key changes.
func TestProvidersPageWizardOptionalProbeFallback(t *testing.T) {
	body := providersHTML
	for _, marker := range []string{
		`wizardProbeStatus=(d&&d.status)||''`,
		`wizardProbeStatus==='key_invalid'`,
		`tdT('providers.wizard.probeUnavailable')`,
		`tdT('providers.wizard.probeNoModels')`,
		`tdT('providers.wizard.probeSchemaUnknown')`,
		`tdT('providers.wizard.keyInvalid'`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("wizard probe fallback missing %q", marker)
		}
	}
	// A successful probe fills the datalist only — never the typed model input.
	if !strings.Contains(body, `dl.innerHTML=models.map(function(m){return '<option value="'+esc(m)+'"></option>'}).join('')`) {
		t.Fatalf("wizard probe must fill the datalist without overwriting typed input")
	}
	// key_invalid blocks the save; the detection-unavailable states do not.
	if !strings.Contains(body, `if(wizardProbeStatus==='key_invalid'){status.className='callout callout--contradicted';status.textContent=tdT('providers.wizard.keyInvalid',{message:''});return}`) {
		t.Fatalf("wizardSave must block on a key_invalid probe")
	}
	// Typing a new key clears the probe verdict so the operator can retry.
	if !strings.Contains(body, `document.getElementById('wizard-key').addEventListener('input',function(){wizardProbeStatus='';`) {
		t.Fatalf("typing a new key must clear the probe verdict")
	}
	// Saving requires endpoint, key and model.
	for _, guard := range []string{
		`tdT('providers.probeNeedEndpoint')`,
		`tdT('providers.probeNeedKey')`,
		`tdT('providers.probeNeedModel')`,
	} {
		if !strings.Contains(body, guard) {
			t.Fatalf("wizardSave missing save guard %q", guard)
		}
	}
}

// One submit creates exactly one channel: wizardSave builds a single body from
// the chosen capability and performs exactly one POST, then clears the key and
// closes only after that POST succeeds — no per-capability fan-out.
func TestProvidersPageWizardOnePostPerSave(t *testing.T) {
	body := providersHTML
	var saveFn string
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "async function wizardSave(") {
			saveFn = line
			break
		}
	}
	if saveFn == "" {
		t.Fatal("wizardSave not found")
	}
	if got := strings.Count(saveFn, `fetch('/api/v1/admin/provider-channels',{method:'POST'`); got != 1 {
		t.Fatalf("wizardSave must perform exactly one channel POST, found %d", got)
	}
	if strings.Contains(saveFn, "for(const cap of caps)") || strings.Contains(saveFn, "created.push") {
		t.Fatalf("wizardSave must not fan out over multiple capabilities")
	}
	if !strings.Contains(saveFn, "capability:cap") || !strings.Contains(saveFn, "members:[{label:'primary'") {
		t.Fatalf("wizardSave must send exactly one channel body with the chosen capability")
	}
	if !strings.Contains(saveFn, `closeDialog('add-provider-dialog')`) {
		t.Fatalf("wizardSave must close the dialog only after the one POST succeeds")
	}
	if !strings.Contains(saveFn, `document.getElementById('wizard-key').value=''`) {
		t.Fatalf("wizardSave must clear the key after the one POST succeeds")
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

// Every product string in the page constant must go through the shared locale
// machinery: static HTML via [[i18n:*]] markers, dynamic copy via the runtime
// helpers, API failures via the shared tdApiErrorMessage, and date output via
// tdFormatDateTime. The page-local apiErrMsg must be gone.
func TestProvidersPageLocalizedCopy(t *testing.T) {
	body := providersHTML
	if strings.Contains(body, "apiErrMsg") {
		t.Fatalf("page-local apiErrMsg must be deleted in favor of the shared tdApiErrorMessage")
	}
	if !strings.Contains(body, "tdApiErrorMessage(") {
		t.Fatalf("providers page must call the shared tdApiErrorMessage helper")
	}
	for _, legacy := range []string{"toLocaleTimeString", "toLocaleString("} {
		if strings.Contains(body, legacy) {
			t.Fatalf("providers page still uses browser-locale formatter %q", legacy)
		}
	}
	if !strings.Contains(body, "tdFormatDateTime(") {
		t.Fatalf("providers page must use the shared tdFormatDateTime formatter")
	}
	// The capability values stay wire data and the select options render from
	// static markers, never from hardcoded labels.
	for _, cap := range []string{"video_analysis", "asr", "embedding", "tag_curator", "repurpose"} {
		if !strings.Contains(body, `<option value="`+cap+`">[[i18n:providers.cap.`+cap+`]]</option>`) {
			t.Fatalf("capability %q option is not rendered from its catalog marker", cap)
		}
	}
}

// Every static [[i18n:key]] marker and every literal key passed to
// tdT/tdPlural (including the value→key enum maps) must resolve: to a key the
// page fragment carries in all five locales, or to one of the shared catalog
// keys (status.* / shell.* / common.*) that every locale already ships. Plural
// bases are resolved through their .one/.other siblings.
func TestProvidersPageI18nKeysResolveFromFragment(t *testing.T) {
	fragKeys := providersFragmentKeys(t)

	markerRE := regexp.MustCompile(`\[\[i18n:([a-zA-Z0-9._-]+)\]\]`)
	callRE := regexp.MustCompile(`td(?:T|Plural)\('([a-zA-Z0-9._-]+)'`)
	keyLitRE := regexp.MustCompile(`'(providers|status|shell|common|api|facet)\.[a-zA-Z0-9._-]+'`)

	seen := make(map[string]bool)
	for _, m := range markerRE.FindAllStringSubmatch(providersHTML, -1) {
		seen[m[1]] = true
	}
	for _, m := range callRE.FindAllStringSubmatch(providersHTML, -1) {
		seen[m[1]] = true
	}
	for _, m := range keyLitRE.FindAllStringSubmatch(providersHTML, -1) {
		seen[m[0][1:len(m[0])-1]] = true
	}

	if len(seen) == 0 {
		t.Fatal("no keys detected in providersHTML; the scan is broken")
	}

	var unresolved []string
	for key := range seen {
		if fragKeys[key] {
			continue
		}
		// A plural base key resolves via its category siblings.
		if fragKeys[key+".one"] || fragKeys[key+".other"] {
			continue
		}
		if catalogs[localeZhCN].has(key) {
			continue
		}
		unresolved = append(unresolved, key)
	}
	sort.Strings(unresolved)
	if len(unresolved) > 0 {
		t.Fatalf("providers keys that resolve to nothing (not in fragment, its plural siblings, or a shared catalog): %v", unresolved)
	}
}

// The value→key capability table must cover every wire value the page renders
// (the channel card, the routing panel, the wizard checkboxes and the dialog
// select). Adding a new capability must fail this test until the map, the
// static markers and the fragment all cover it.
func TestProvidersPageI18nEnumMapsCoverWireValues(t *testing.T) {
	body := providersHTML
	caps := []string{"video_analysis", "asr", "embedding", "tag_curator", "repurpose"}
	for _, cap := range caps {
		if !strings.Contains(body, cap+":'providers.cap."+cap+"'") {
			t.Fatalf("capKeys missing capability %q", cap)
		}
		if !strings.Contains(body, `<option value="`+cap+`">[[i18n:providers.cap.`+cap+`]]</option>`) {
			t.Fatalf("capability %q select option missing its marker", cap)
		}
	}
	if !strings.Contains(body, "function capName(cap){return tdT(capKeys[cap]||cap)}") {
		t.Fatalf("capName must render capability labels through the capKeys table with a raw fallback")
	}
}

func TestProvidersPageI18nFragmentParityAcrossLocales(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("locales", "fragments", "providers.json"))
	if err != nil {
		t.Fatal(err)
	}
	var frag struct {
		Page string                       `json:"page"`
		Keys map[string]map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(data, &frag); err != nil {
		t.Fatal(err)
	}
	if frag.Page != "providers" {
		t.Fatalf("fragment page=%q, want providers", frag.Page)
	}
	for _, loc := range supportedLocales {
		if _, ok := frag.Keys[string(loc)]; !ok {
			t.Fatalf("fragment missing locale %s", loc)
		}
	}

	base := frag.Keys[string(localeZhCN)]
	for _, loc := range supportedLocales[1:] {
		other := frag.Keys[string(loc)]
		if len(other) != len(base) {
			t.Fatalf("locale %s has %d keys, zh-CN has %d", loc, len(other), len(base))
		}
		for k := range base {
			if _, ok := other[k]; !ok {
				t.Fatalf("locale %s missing key %q", loc, k)
			}
		}
	}

	phRE := regexp.MustCompile(`\{[a-zA-Z]+\}`)
	phSet := func(s string) string {
		parts := phRE.FindAllString(s, -1)
		sort.Strings(parts)
		return strings.Join(parts, ",")
	}
	for k, zh := range base {
		want := phSet(zh)
		for _, loc := range supportedLocales[1:] {
			if got := phSet(frag.Keys[string(loc)][k]); got != want {
				t.Fatalf("placeholder set differs for %s in %s: %q vs zh-CN %q", k, loc, frag.Keys[string(loc)][k], zh)
			}
		}
	}

	// The fragment must define page-prefixed keys only, never the shared ones.
	for k := range base {
		for _, prefix := range []string{"common.", "api.", "status.", "facet.", "shell."} {
			if strings.HasPrefix(k, prefix) {
				t.Fatalf("fragment redefines shared key %q", k)
			}
		}
	}
}

// Served in the default zh-CN locale, every static marker must be resolved by
// the server (to the zh-CN value once the fragment is merged, to the bare key
// before that) rather than leaking as [[i18n:...]], and the structural anchors
// the other pages and tests rely on must survive.
func TestProvidersPageI18nServedWithoutUnresolvedMarkers(t *testing.T) {
	service := providerChannelTestService(t, "providers-served-page.db")
	handler := NewServer("", service).Handler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, lanRequest(http.MethodGet, "/providers", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if strings.Contains(body, "[[i18n:") {
		t.Fatalf("unresolved marker leaked into the served page: %s", body)
	}
	for _, want := range []string{
		`<html lang="zh-CN"`,
		`data-app-shell`,
		`id="new-channel-btn"`, `id="advanced-btn"`, `id="refresh-channels"`,
		`id="channels"`, `id="routing"`, `id="provider-dialog"`, `id="add-provider-dialog"`,
		`id="channel-form"`, `id="capability"`, `id="provider-name"`, `id="label"`,
		`id="model"`, `id="endpoint"`, `id="route-order"`, `id="member-rows"`,
		`id="api-key"`, `id="form-status"`, `id="wizard-provider"`, `id="wizard-key"`,
		`id="wizard-model"`, `id="wizard-detect"`, `id="wizard-save"`, `<header class="pagehead">`,
		`id="channels-status"`, `id="provider-health"`, `id="auth-callout"`,
		`data-act="test-channel"`, `data-act="edit-channel"`, `data-act="toggle-channel"`,
		`data-act="delete-channel"`, `data-act="toggle-add"`, `data-act="save-key"`,
		`data-act="cancel-key"`, `data-act="remove-member"`,
		`/api/v1/admin/provider-channels/status`, `/api/v1/admin/provider-channels/probe-models`,
		`/api/v1/admin/provider-channels/'+encodeURIComponent(id)`,
		`method:'PATCH'`,
		`admin-token`, `loginAdmin`, `X-CSRF-Token`, `__Host-nexusslate_csrf`,
		`tdApiErrorMessage`, `tdFormatDateTime`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("served /providers missing %q", want)
		}
	}
}

// The advanced provider options carry wire values only; every label renders
// through providerNameKeys with the selected catalog, so no mixed-language raw
// string lives in the page (local_vlm, qwen_video, 火山 etc. all resolve via
// the catalog). The key map must cover every provider the page can offer.
func TestProvidersPageProviderLabelsLocalized(t *testing.T) {
	body := providersHTML
	if !strings.Contains(body, `function syncProviderOptions(){const capability=document.getElementById('capability').value;const select=document.getElementById('provider-name');const prior=select.value;select.innerHTML=(providerOptions[capability]||[]).map((value)=>'<option value="'+esc(value)+'">'+esc(tdT(providerNameKeys[value]||value))+'</option>')`) {
		t.Fatalf("advanced provider options must render through providerNameKeys")
	}
	for _, wire := range []string{
		"gemini", "qwen_video", "volcengine_video", "local_vlm", "stepfun",
		"qwen", "volcengine_asr", "openai_embeddings", "gemini_embed_content",
		"volc_agent_plan_embedding", "volc_coding_plan_embedding", "openai_chat",
		"volc_agent_plan", "volc_coding_plan",
	} {
		if !strings.Contains(body, wire+":'providers.provider."+wire+"'") {
			t.Fatalf("providerNameKeys missing %q", wire)
		}
	}
	// No raw mixed-language label survives in the options map: the map is
	// wire-only arrays, so no '千问' / '火山' / '本地' literal should appear in it.
	if strings.Contains(body, "千问视频'") || strings.Contains(body, "本地 VLM'") {
		t.Fatalf("advanced provider options must not carry raw mixed-language labels")
	}
}
