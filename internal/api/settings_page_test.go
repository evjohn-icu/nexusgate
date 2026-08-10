package api

import (
	"context"
	"encoding/json"
	"fmt"
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

func throttleTestService(t *testing.T, name string) *app.Service {
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

// The settings page is the only place these limits are discoverable, so the
// throttle must be readable without a token from the LAN while changing it stays
// administrative.
func TestThrottleEndpointReadableWithoutTokenAndWritableOnlyByAdmin(t *testing.T) {
	service := throttleTestService(t, "throttle-api.db")
	handler := NewServer("", service).Handler()

	read := httptest.NewRecorder()
	handler.ServeHTTP(read, lanRequest(http.MethodGet, "/api/v1/pipeline/throttle", nil))
	if read.Code != http.StatusOK {
		t.Fatalf("read status=%d body=%s", read.Code, read.Body.String())
	}
	var initial struct {
		Throttle    domain.PipelineThrottle `json:"throttle"`
		OffPeakOpen bool                    `json:"off_peak_open"`
		ServerZone  string                  `json:"server_zone"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &initial); err != nil {
		t.Fatal(err)
	}
	// A fresh install must be unthrottled: turning this on by default would
	// silently slow every existing library after an upgrade.
	if initial.Throttle.ReadRate != 0 || initial.Throttle.CooldownSeconds != 0 || initial.Throttle.OffPeakEnabled {
		t.Fatalf("default must be unthrottled: %+v", initial.Throttle)
	}
	if !initial.OffPeakOpen {
		t.Fatal("a disabled window must report as open, or held work would look stranded")
	}
	// The window is judged in the Hub's zone, so the page has to be able to say
	// which zone that is.
	if !strings.HasPrefix(initial.ServerZone, "UTC") {
		t.Fatalf("server_zone=%q should name the Hub offset", initial.ServerZone)
	}

	body := `{"read_rate":2,"cooldown_seconds":15,"off_peak_enabled":true,"off_peak_start":"01:00","off_peak_end":"07:00","defer_above_bytes":524288000,"immediate_max_bytes":52428800}`
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, lanRequest(http.MethodPut, "/api/v1/pipeline/throttle", strings.NewReader(body)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("write without a token status=%d", unauthorized.Code)
	}

	saved := httptest.NewRecorder()
	handler.ServeHTTP(saved, hubAdminRequest(service, http.MethodPut, "/api/v1/pipeline/throttle", strings.NewReader(body)))
	if saved.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", saved.Code, saved.Body.String())
	}
	var stored struct {
		Throttle     domain.PipelineThrottle `json:"throttle"`
		HoldingAbove int64                   `json:"holding_above"`
		NextOffPeak  string                  `json:"next_off_peak"`
	}
	if err := json.Unmarshal(saved.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Throttle.ReadRate != 2 || stored.Throttle.CooldownSeconds != 15 || stored.Throttle.DeferAboveBytes != 524288000 {
		t.Fatalf("save did not round-trip: %+v", stored.Throttle)
	}
	if stored.NextOffPeak == "" {
		t.Fatal("an enabled window must report when it next opens; the page shows it instead of leaving the queue looking stuck")
	}
}

// Every rejection must leave the stored value alone. A partially applied throttle
// would throttle at values the operator never chose.
func TestThrottleValidationRejectionsLeaveStoredValueIntact(t *testing.T) {
	service := throttleTestService(t, "throttle-reject.db")
	handler := NewServer("", service).Handler()

	good := `{"read_rate":2,"cooldown_seconds":10}`
	accepted := httptest.NewRecorder()
	handler.ServeHTTP(accepted, hubAdminRequest(service, http.MethodPut, "/api/v1/pipeline/throttle", strings.NewReader(good)))
	if accepted.Code != http.StatusOK {
		t.Fatalf("baseline save status=%d body=%s", accepted.Code, accepted.Body.String())
	}

	for name, payload := range map[string]string{
		"read rate below the floor":    `{"read_rate":0.1}`,
		"negative read rate":           `{"read_rate":-1}`,
		"cooldown beyond the limit":    `{"cooldown_seconds":9000}`,
		"exemption above the deferral": `{"immediate_max_bytes":900,"defer_above_bytes":100}`,
		"empty window":                 `{"off_peak_enabled":true,"off_peak_start":"03:00","off_peak_end":"03:00"}`,
		"malformed clock":              `{"off_peak_enabled":true,"off_peak_start":"25:99","off_peak_end":"07:00"}`,
		"not json":                     `{`,
		"negative deferral":            `{"defer_above_bytes":-5}`,
		"negative free space floor":    `{"minimum_free_space_bytes":-5}`,
		"negative daily cost guide":    `{"daily_cost_guide":-5}`,
		"negative monthly cost guide":  `{"monthly_cost_guide":-5}`,
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPut, "/api/v1/pipeline/throttle", strings.NewReader(payload)))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s (want 400)", name, response.Code, response.Body.String())
		}
	}

	after := httptest.NewRecorder()
	handler.ServeHTTP(after, lanRequest(http.MethodGet, "/api/v1/pipeline/throttle", nil))
	var stored struct {
		Throttle domain.PipelineThrottle `json:"throttle"`
	}
	if err := json.Unmarshal(after.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Throttle.ReadRate != 2 || stored.Throttle.CooldownSeconds != 10 {
		t.Fatalf("a rejected write changed the stored throttle: %+v", stored.Throttle)
	}
}

// The disk-space panel shares the throttle's save/load endpoints, so its
// value must survive an admin write and a tokenless read exactly like the
// load levers — and the page must render it back in GB.
func TestSettingsDiskSpaceProtectionRoundTripsInGB(t *testing.T) {
	service := throttleTestService(t, "settings-disk.db")
	handler := NewServer("", service).Handler()

	// 5 GB in bytes; the page converts with GB=1073741824 before sending.
	const fiveGB = int64(5) * 1073741824
	body := `{"minimum_free_space_bytes":` + fmt.Sprint(fiveGB) + `}`
	saved := httptest.NewRecorder()
	handler.ServeHTTP(saved, hubAdminRequest(service, http.MethodPut, "/api/v1/pipeline/throttle", strings.NewReader(body)))
	if saved.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", saved.Code, saved.Body.String())
	}
	var stored struct {
		Throttle domain.PipelineThrottle `json:"throttle"`
	}
	if err := json.Unmarshal(saved.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Throttle.MinimumFreeSpaceBytes != fiveGB {
		t.Fatalf("save did not round-trip minimum_free_space_bytes: %+v", stored.Throttle)
	}

	read := httptest.NewRecorder()
	handler.ServeHTTP(read, lanRequest(http.MethodGet, "/api/v1/pipeline/throttle", nil))
	if read.Code != http.StatusOK {
		t.Fatalf("read status=%d body=%s", read.Code, read.Body.String())
	}
	if err := json.Unmarshal(read.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Throttle.MinimumFreeSpaceBytes != fiveGB {
		t.Fatalf("the LAN read must show the persisted floor: %+v", stored.Throttle)
	}

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/settings", nil))
	for _, marker := range []string{
		"磁盘空间保护", // the panel itself
		"min-free-space",
		"minimum_free_space_bytes", // the field both directions carry
		"GB=1073741824",            // and the bytes conversion the page promises
	} {
		if !strings.Contains(page.Body.String(), marker) {
			t.Fatalf("settings page is missing %q", marker)
		}
	}
}

// The cost-guide panel shares the throttle's save/load endpoints, so its
// values must survive an admin write and a tokenless read exactly like the
// disk panel's — and the page must render both fields.
func TestSettingsCostGuidePanelRoundTrips(t *testing.T) {
	service := throttleTestService(t, "settings-cost-guide.db")
	handler := NewServer("", service).Handler()

	const daily, monthly = 25.5, 300
	body := fmt.Sprintf(`{"daily_cost_guide":%v,"monthly_cost_guide":%v}`, daily, monthly)
	saved := httptest.NewRecorder()
	handler.ServeHTTP(saved, hubAdminRequest(service, http.MethodPut, "/api/v1/pipeline/throttle", strings.NewReader(body)))
	if saved.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", saved.Code, saved.Body.String())
	}
	var stored struct {
		Throttle domain.PipelineThrottle `json:"throttle"`
	}
	if err := json.Unmarshal(saved.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Throttle.DailyCostGuide != daily || stored.Throttle.MonthlyCostGuide != monthly {
		t.Fatalf("save did not round-trip the cost guides: %+v", stored.Throttle)
	}

	read := httptest.NewRecorder()
	handler.ServeHTTP(read, lanRequest(http.MethodGet, "/api/v1/pipeline/throttle", nil))
	if read.Code != http.StatusOK {
		t.Fatalf("read status=%d body=%s", read.Code, read.Body.String())
	}
	if err := json.Unmarshal(read.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Throttle.DailyCostGuide != daily || stored.Throttle.MonthlyCostGuide != monthly {
		t.Fatalf("the LAN read must show the persisted cost guides: %+v", stored.Throttle)
	}

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/settings", nil))
	for _, marker := range []string{
		"成本参考", // the panel itself
		"daily-cost-guide",
		"monthly-cost-guide",
		"daily_cost_guide", // the field both directions carry
		"monthly_cost_guide",
		"不会限制或推迟", // guides never park work
	} {
		if !strings.Contains(page.Body.String(), marker) {
			t.Fatalf("settings page is missing %q", marker)
		}
	}
}

func TestSettingsPageCarriesTokenPlumbingAndBothLevers(t *testing.T) {
	service := throttleTestService(t, "settings-page.db")
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	page := response.Body.String()
	for _, marker := range []string{
		"admin-token", // the token input
		"authHeaders", // and it is actually attached to requests
		"/api/v1/pipeline/throttle",
		"read-rate", "cooldown", "off-peak", "defer-mb", "immediate-mb",
		"server-time", "server-zone", // the window is judged in the Hub's zone
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("settings page is missing %q", marker)
		}
	}
	// The token must never be persisted by the browser; that promise is repeated
	// on every page in this project.
	if strings.Contains(page, "localStorage") || strings.Contains(page, "sessionStorage") {
		t.Fatal("settings page must keep the admin token in page memory only")
	}
}
