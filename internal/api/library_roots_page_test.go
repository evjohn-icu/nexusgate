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
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/mount"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

func newLibraryRootsTestService(t *testing.T, name string) *app.Service {
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

// The wizard exists because /setup told operators to "add a mount directory
// on the progress page" and /progress never had such a form -- a promise the
// UI did not keep. This checks the page actually renders its four steps and,
// like every other admin page here, never persists the Hub token.
func TestLibraryRootsPageRendersWithStepMarkers(t *testing.T) {
	service := newLibraryRootsTestService(t, "library-roots-page.db")
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/library-roots", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if ct := response.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("content-type=%q", ct)
	}
	body := response.Body.String()
	for _, marker := range []string{
		"data-library-roots-wizard",
		"输入路径或共享",
		"挂载说明",
		"验证挂载",
		"添加并扫描",
		"admin-token",
		`type="password"`,
		"/api/v1/roots/inspect",
		"compose-section",
		"Docker Compose",
		"compose_volume",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("page missing marker %q", marker)
		}
	}
	// The token must never be persisted by the browser; that promise is
	// repeated on every admin page in this project.
	if strings.Contains(body, "localStorage") || strings.Contains(body, "sessionStorage") {
		t.Fatal("library roots page must keep the admin token in page memory only")
	}
}

// CLAUDE.md requires product/UI copy to be Chinese while internal/mount
// stays English -- it is also the CLI's doctor output. This wizard bridges
// that by translating mount.Step/mount.Note by their stable Key in a table
// baked into this page. A step or note whose Key was added to mount but
// never given an entry in that table would silently render the raw English
// text inside an otherwise fully Chinese page, which is exactly the defect
// this whole feature exists to fix. Rather than hardcoding the key list
// here -- which would stop catching anything the day a new step or note is
// added -- this walks every host/share combination mount.Guidance can
// currently produce and derives the key set from it, so a newly introduced
// step or note without a translation fails this test immediately.
func TestLibraryRootsPageTranslatesEveryGuidanceKey(t *testing.T) {
	smb, ok := mount.ParseShare("//192.0.2.10/Video")
	if !ok {
		t.Fatal("smb fixture did not parse as a share")
	}
	nfs, ok := mount.ParseShare("192.0.2.10:/volume1/Video")
	if !ok {
		t.Fatal("nfs fixture did not parse as a share")
	}
	hosts := []mount.Host{
		{OS: "linux", UID: 1000, GID: 1000},
		{OS: "linux", UID: 1000, GID: 1000, WSL: true},
		{OS: "darwin"},
		{OS: "windows"},
	}
	keys := map[string]bool{}
	for _, share := range []mount.Share{smb, nfs} {
		for _, host := range hosts {
			guide := mount.Guidance(share, "", host)
			for _, step := range guide.Steps {
				keys[step.Key] = true
			}
			for _, note := range guide.Notes {
				keys[note.Key] = true
			}
		}
	}
	if len(keys) == 0 {
		t.Fatal("derived no keys from mount.Guidance -- this test is not exercising the page at all")
	}
	for key := range keys {
		if key == "" {
			t.Fatal("mount.Guidance produced an empty key; internal/mount's own test should have caught this first")
		}
		marker := "'" + key + "':'"
		idx := strings.Index(libraryRootsHTML, marker)
		if idx < 0 {
			t.Errorf("page has no MOUNT_TR entry for guidance key %q", key)
			continue
		}
		start := idx + len(marker)
		end := strings.Index(libraryRootsHTML[start:], "'")
		if end < 0 {
			t.Errorf("MOUNT_TR entry for %q is not closed by a following quote", key)
			continue
		}
		translation := libraryRootsHTML[start : start+end]
		hasCJK := false
		for _, r := range translation {
			if r >= 0x4E00 && r <= 0x9FFF {
				hasCJK = true
				break
			}
		}
		if !hasCJK {
			t.Errorf("MOUNT_TR entry for %q = %q does not look like a Chinese translation", key, translation)
		}
	}
}

// mount.ComposeVolume's WarningKey follows the exact same translation
// contract as mount.Step.Key/mount.Note.Key (see ComposeVolumeSuggestion in
// internal/app/service.go), but it is not reachable from mount.Guidance, so
// TestLibraryRootsPageTranslatesEveryGuidanceKey above -- which only walks
// Guidance's steps and notes -- cannot see it. A key introduced on the
// compose side without an entry here would silently render English inside
// the danger box a password warning belongs in, which is exactly the
// scenario the whole feature exists to avoid.
func TestLibraryRootsPageTranslatesComposeWarningKey(t *testing.T) {
	smb, ok := mount.ParseShare("//192.0.2.10/Video")
	if !ok {
		t.Fatal("smb fixture did not parse as a share")
	}
	volume, ok := mount.ComposeVolume(smb, "nas-video")
	if !ok {
		t.Fatal("an SMB share must produce a compose volume definition")
	}
	if volume.WarningKey == "" {
		t.Fatal("the SMB compose form must carry a WarningKey; internal/mount's own test should have caught this first")
	}
	marker := "'" + volume.WarningKey + "':'"
	idx := strings.Index(libraryRootsHTML, marker)
	if idx < 0 {
		t.Fatalf("page has no MOUNT_TR entry for compose warning key %q", volume.WarningKey)
	}
	start := idx + len(marker)
	end := strings.Index(libraryRootsHTML[start:], "'")
	if end < 0 {
		t.Fatalf("MOUNT_TR entry for %q is not closed by a following quote", volume.WarningKey)
	}
	translation := libraryRootsHTML[start : start+end]
	hasCJK := false
	for _, r := range translation {
		if r >= 0x4E00 && r <= 0x9FFF {
			hasCJK = true
			break
		}
	}
	if !hasCJK {
		t.Errorf("MOUNT_TR entry for %q = %q does not look like a Chinese translation", volume.WarningKey, translation)
	}
}

// An operator who has already copied the YAML block has already leaked the
// password it would have warned about; the warning is only useful read
// first. This checks the client-side rendering code itself puts the danger
// box before the copyable YAML block in renderComposeVolume, not after.
func TestLibraryRootsPageRendersComposeWarningAboveYAML(t *testing.T) {
	start := strings.Index(libraryRootsHTML, "function renderComposeVolume")
	if start < 0 {
		t.Fatal("renderComposeVolume is not defined on the page")
	}
	end := strings.Index(libraryRootsHTML[start:], "\n}")
	if end < 0 {
		t.Fatal("could not find the end of renderComposeVolume")
	}
	body := libraryRootsHTML[start : start+end]
	warningIdx := strings.Index(body, "danger")
	yamlIdx := strings.Index(body, "compose-yaml-code")
	if warningIdx < 0 {
		t.Fatal("renderComposeVolume never renders the danger box")
	}
	if yamlIdx < 0 {
		t.Fatal("renderComposeVolume never renders the YAML block")
	}
	if warningIdx > yamlIdx {
		t.Fatal("the warning must be emitted before the YAML block, not after — see the danger box's placement rationale")
	}
}

// The decision doc's actual recommendation is to avoid the cleartext-password
// problem entirely by using NFS instead of SMB when the NAS supports it, not
// merely to disclose the caveat and move on.
func TestLibraryRootsPageRecommendsNFSOverSMBForCompose(t *testing.T) {
	start := strings.Index(libraryRootsHTML, "function renderComposeVolume")
	if start < 0 {
		t.Fatal("renderComposeVolume is not defined on the page")
	}
	end := strings.Index(libraryRootsHTML[start:], "\n}")
	body := libraryRootsHTML[start : start+end]
	if !strings.Contains(body, "protocol==='smb'") {
		t.Fatal("renderComposeVolume must branch on the SMB case to offer the NFS alternative")
	}
	if !strings.Contains(body, "NFS") {
		t.Fatal("the SMB branch must recommend NFS as the way to avoid the cleartext-credentials problem entirely")
	}
}

// This is the important one: /api/v1/roots/inspect answers os.Stat and mount
// table questions about an arbitrary path on the Hub's own filesystem. If it
// were reachable without the admin token, any device on the LAN could probe
// which paths exist on the Hub.
func TestInspectRootRejectsUnauthenticatedRequest(t *testing.T) {
	service := newLibraryRootsTestService(t, "inspect-auth.db")
	handler := NewServer("", service).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, lanRequest(http.MethodPost, "/api/v1/roots/inspect", strings.NewReader(`{"path":"/tmp"}`)))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s, want 401 — an unauthenticated LAN caller must not be able to probe Hub filesystem paths", response.Code, response.Body.String())
	}
}

// A share address like //nas.local/Video should come back with the exact
// mount commands (mount.Guidance), not just "not found" — that is the whole
// point of the endpoint over a bare os.Stat.
func TestInspectRootShareInputReturnsGuidance(t *testing.T) {
	service := newLibraryRootsTestService(t, "inspect-share.db")
	handler := NewServer("", service).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/roots/inspect", strings.NewReader(`{"path":"//nas.local/Video"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var inspection app.RootInspection
	if err := json.Unmarshal(response.Body.Bytes(), &inspection); err != nil {
		t.Fatal(err)
	}
	if !inspection.IsShare {
		t.Fatalf("//nas.local/Video should parse as a share: %+v", inspection)
	}
	if inspection.Share == nil || inspection.Share.Host != "nas.local" || inspection.Share.Name != "Video" {
		t.Fatalf("share=%+v", inspection.Share)
	}
	if inspection.Guidance == nil || inspection.Guidance.Summary == "" || len(inspection.Guidance.Steps) == 0 {
		t.Fatalf("guidance missing or empty: %+v", inspection.Guidance)
	}
	if inspection.DefaultMountpoint == "" {
		t.Fatal("default_mountpoint must be prefilled so the wizard's step 2 has something to show before the operator edits it")
	}
}

// Dropping the password out of Share.User is not enough on its own: this
// response is rendered straight into the page that asked for it, so anything
// in it that still echoes what was typed puts the password back in the DOM.
// A browser address bar accepts smb://user:password@host/share, which is
// exactly the string an operator is liable to paste. The whole response body
// is checked rather than one field, because the leak this covers was in
// `path` — a field nobody thought of as carrying a credential.
func TestInspectRootNeverEchoesAPastedPassword(t *testing.T) {
	service := newLibraryRootsTestService(t, "inspect-password.db")
	handler := NewServer("", service).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/roots/inspect", strings.NewReader(`{"path":"smb://ev:hunter2@nas.local/Video"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "hunter2") {
		t.Fatalf("the pasted password reached the response body: %s", response.Body.String())
	}
	var inspection app.RootInspection
	if err := json.Unmarshal(response.Body.Bytes(), &inspection); err != nil {
		t.Fatal(err)
	}
	// The username is still useful — it prefills the credentials file — so
	// this must not have been achieved by discarding the userinfo wholesale.
	if inspection.Share == nil || inspection.Share.User != "ev" {
		t.Fatalf("the username must survive so the generated commands can use it: %+v", inspection.Share)
	}
}

// This test machine is never itself the containerised Hub the compose_volume
// suggestion is for (see app.Service.hostOverride's doc), and this package
// cannot reach that unexported field to fake it — only internal/app can, in
// TestInspectRootPathComposeVolumeNeverEchoesAPastedPassword, which is the
// version of this check that actually exercises a populated compose_volume.
// This one only guards the ordinary path/share fields over a real HTTP
// response, kept here as a regression guard alongside the other
// password-leak tests in this file.
func TestInspectRootShareResponseNeverEchoesAPastedPasswordOverHTTP(t *testing.T) {
	service := newLibraryRootsTestService(t, "inspect-password-http.db")
	handler := NewServer("", service).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/roots/inspect", strings.NewReader(`{"path":"smb://ev:hunter2@192.0.2.10/Video"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "hunter2") {
		t.Fatalf("the pasted password reached the response body: %s", response.Body.String())
	}
}

// The same leak reaches an API caller that never opens the wizard, through
// the 422 body createRoot answers an unmounted share with.
func TestCreateRootNeverEchoesAPastedPassword(t *testing.T) {
	service := newLibraryRootsTestService(t, "create-password.db")
	handler := NewServer("", service).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/roots", strings.NewReader(`{"path":"smb://ev:hunter2@nas.local/Video"}`)))
	if strings.Contains(response.Body.String(), "hunter2") {
		t.Fatalf("the pasted password reached the response body: %s", response.Body.String())
	}
}

// A plain, already-accessible directory is the common case and must not be
// treated as a share (no guidance) or flagged with warnings that only apply
// to network mounts.
func TestInspectRootPlainDirectoryReturnsNoGuidanceOrWarnings(t *testing.T) {
	service := newLibraryRootsTestService(t, "inspect-plain-dir.db")
	handler := NewServer("", service).Handler()
	dir := t.TempDir()
	payload, err := json.Marshal(map[string]string{"path": dir})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/roots/inspect", strings.NewReader(string(payload))))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var inspection app.RootInspection
	if err := json.Unmarshal(response.Body.Bytes(), &inspection); err != nil {
		t.Fatal(err)
	}
	if inspection.IsShare || inspection.Share != nil || inspection.Guidance != nil {
		t.Fatalf("a plain local directory must not be treated as a share: %+v", inspection)
	}
	if !inspection.Exists || !inspection.IsDir {
		t.Fatalf("an existing temp directory should report exists=true, is_dir=true: %+v", inspection)
	}
	if len(inspection.Warnings) != 0 {
		t.Fatalf("a plain local temp directory should carry no warnings, got %v", inspection.Warnings)
	}
}

// A typo'd or not-yet-created path must be reported, not turned into a 500 —
// the wizard's step 3 relies on this to tell the operator "not there yet,
// try again" rather than showing a crash.
func TestInspectRootNonExistentPathReportsInsteadOfFailing(t *testing.T) {
	service := newLibraryRootsTestService(t, "inspect-missing.db")
	handler := NewServer("", service).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/roots/inspect", strings.NewReader(`{"path":"/definitely/does/not/exist/on/this/machine-xyz"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 — a missing path is reported, not a server error", response.Code, response.Body.String())
	}
	var inspection app.RootInspection
	if err := json.Unmarshal(response.Body.Bytes(), &inspection); err != nil {
		t.Fatal(err)
	}
	if inspection.Exists {
		t.Fatalf("a path that does not exist must report exists=false: %+v", inspection)
	}
}

// Before this change, createRoot's ErrShareNotMounted handling fell through to
// writeError, which always answers 500 with just err.Error() as plain text.
// An API caller (or the wizard, on an unlucky race where the share unmounts
// between verify and add) needs to tell "share not mounted" apart from any
// other failure and get the guidance back, not a generic error string.
func TestCreateRootWithUnmountedShareReturnsShareSpecificResponse(t *testing.T) {
	service := newLibraryRootsTestService(t, "create-root-unmounted-share.db")
	handler := NewServer("", service).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/roots", strings.NewReader(`{"path":"//nas.local/Video"}`)))
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422 — an unmounted share must not look like the same generic failure as a bad local path", response.Code, response.Body.String())
	}
	var payload struct {
		Error      string             `json:"error"`
		Inspection app.RootInspection `json:"inspection"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error == "" {
		t.Fatal("response must carry an error message")
	}
	if !payload.Inspection.IsShare || payload.Inspection.Guidance == nil {
		t.Fatalf("response must carry the mount guidance, so an API caller that never opens /library-roots is still told what to do: %+v", payload.Inspection)
	}
}
