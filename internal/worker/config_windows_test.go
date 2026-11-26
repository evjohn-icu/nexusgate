//go:build windows

package worker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The Windows-native acceptance for Worker config privacy: a config written by
// this package carries a protected DACL granting only the running account,
// LocalSystem and Administrators; files whose DACL lets other accounts read or
// write the token are rejected without touching their contents; and an
// already enrolled config can be replaced.

func testRunningSID(t *testing.T) *windows.SID {
	t.Helper()
	sid, err := runningAccountSID()
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

// applyTestSDDL replaces the DACL at path with the one the SDDL string
// describes, so a test can build a security state the Worker must reject.
func applyTestSDDL(t *testing.T, path, sddl string) {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatalf("parse test SDDL %q: %v", sddl, err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	protection := windows.SECURITY_INFORMATION(windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if strings.HasPrefix(sddl, "D:P") {
		protection = windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|protection,
		nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

func makeExistingConfigInsecure(t *testing.T, path string) {
	t.Helper()
	applyTestSDDL(t, path, "D:P(A;;FA;;;"+testRunningSID(t).String()+")(A;;GR;;;WD)")
}

// assertConfigStillSecure asserts the exact state LoadConfig accepts after a
// local-admin update: an owner-controlled file whose DACL grants read, write
// or DACL control only to the running account, LocalSystem and Administrators
// (POSIX permission bits carry no meaning on Windows).
func assertConfigStillSecure(t *testing.T, path string) {
	t.Helper()
	if err := validateConfigSecurity(path); err != nil {
		t.Fatalf("published Windows config no longer protected: %v", err)
	}
}

// assertWorkerDACL asserts that path carries the protected Worker DACL: an
// allowed owner and no ACE granting any other account read, write or DACL
// control.
func assertWorkerDACL(t *testing.T, path string) {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		t.Fatal(err)
	}
	if !isConfigPrincipal(owner) {
		t.Fatalf("owner %q is not the running account, LocalSystem or Administrators", owner.String())
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("published DACL is not protected from inherited access")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if dacl == nil {
		t.Fatal("published DACL is absent")
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		if ace.Mask&configSensitiveAccess == 0 {
			continue
		}
		trustee := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !isConfigPrincipal(trustee) {
			t.Fatalf("published DACL grants sensitive access to %q", trustee.String())
		}
	}
}

func windowsConfigFixture(t *testing.T, contents string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "worker.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigWindowsRejectsInsecureInheritedDACL(t *testing.T) {
	path := windowsConfigFixture(t, `{"hub_url":"https://original","token":"original"}`)
	// A freshly written file inherits its DACL; force the case explicitly by
	// granting Everyone full access without the protected flag.
	applyTestSDDL(t, path, "D:(A;OICI;FA;;;WD)")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{HubURL: "https://worker", Token: "node-token"}
	if err := PrepareConfigPath(path); err == nil {
		t.Fatal("expected PrepareConfigPath to reject insecure access before enrollment")
	}
	if err := SaveConfig(path, config); err == nil {
		t.Fatal("expected SaveConfig to reject insecure access")
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected LoadConfig to reject insecure access")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("rejected save changed file: got %q want %q", got, original)
	}
}

func TestConfigWindowsUpgradesSecureInheritedDACL(t *testing.T) {
	dir := t.TempDir()
	applyTestSDDL(t, dir, "D:P(A;OICI;FA;;;"+testRunningSID(t).String()+")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	path := filepath.Join(dir, "worker.json")
	if err := os.WriteFile(path, []byte(`{"hub_url":"https://original","token":"original"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("load config with trusted inherited rights: %v", err)
	}
	if err := SaveConfig(path, Config{HubURL: "https://worker", Token: "next"}); err != nil {
		t.Fatal(err)
	}
	assertWorkerDACL(t, path)
}

func TestConfigWindowsRejectsOtherAccountAccess(t *testing.T) {
	running := testRunningSID(t)
	cases := []struct {
		name string
		sddl string
	}{
		{name: "everyone full", sddl: "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + running.String() + ")(A;OICI;FA;;;WD)"},
		{name: "users read", sddl: "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + running.String() + ")(A;OICI;FR;;;BU)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := windowsConfigFixture(t, `{"hub_url":"https://original","token":"original"}`)
			applyTestSDDL(t, path, tc.sddl)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			config := Config{HubURL: "https://worker", Token: "node-token"}
			err = PrepareConfigPath(path)
			if err == nil {
				t.Fatal("expected PrepareConfigPath to reject access for other accounts")
			}
			if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), filepath.Base(path)) {
				t.Fatalf("insecure destination error must not name the path: %v", err)
			}
			if err := SaveConfig(path, config); err == nil {
				t.Fatal("expected SaveConfig to reject access for other accounts")
			}
			if _, err := LoadConfig(path); err == nil {
				t.Fatal("expected LoadConfig to reject access for other accounts")
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(original) {
				t.Fatalf("rejected save changed file: got %q want %q", got, original)
			}
		})
	}
}

func TestConfigWindowsAcceptsProtectedFileAndReplaces(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "worker.json")
	first := Config{HubURL: "https://one", Token: "token-one"}
	if err := SaveConfig(path, first); err != nil {
		t.Fatal(err)
	}
	assertWorkerDACL(t, path)

	zero := int64(0)
	second := Config{HubURL: "https://two", Token: "token-two", SourceCacheMaxBytes: &zero}
	if err := SaveConfig(path, second); err != nil {
		t.Fatalf("replacing an already enrolled config must succeed: %v", err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.HubURL != "https://two" || got.Token != "token-two" || got.SourceCacheCap() != 0 {
		t.Fatalf("got=%+v", got)
	}
	assertWorkerDACL(t, path)
	if err := PrepareConfigPath(path); err != nil {
		t.Fatalf("re-enrollment must accept the replaced config: %v", err)
	}
}

func TestConfigWindowsCreatesProtectedParentsAndPermitsExistingDirs(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "created", "deep", "worker.json")
	config := Config{HubURL: "https://worker", Token: "node-token"}
	if err := PrepareConfigPath(missing); err != nil {
		t.Fatalf("PrepareConfigPath must create missing parents: %v", err)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("PrepareConfigPath must not create the destination itself: %v", err)
	}
	if err := SaveConfig(missing, config); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(missing); err != nil {
		t.Fatal(err)
	}
	assertWorkerDACL(t, missing)
	assertWorkerDACL(t, filepath.Dir(missing))

	existing := filepath.Join(t.TempDir(), "existing")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(existing, "worker.json")
	if err := SaveConfig(managed, config); err != nil {
		t.Fatalf("existing directory must be permitted when the child config DACL can be protected: %v", err)
	}
	if _, err := LoadConfig(managed); err != nil {
		t.Fatal(err)
	}
	assertWorkerDACL(t, managed)
}

func TestConfigWindowsRejectsSymlinkDestination(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "worker.json")
	if err := os.Symlink(target, path); err != nil {
		var errno windows.Errno
		if errors.As(err, &errno) && errno == windows.ERROR_PRIVILEGE_NOT_HELD {
			t.Skip("file symlink creation requires Windows privilege")
		}
		t.Fatal(err)
	}
	config := Config{HubURL: "https://worker", Token: "node-token"}
	if err := PrepareConfigPath(path); err == nil {
		t.Fatal("expected symlink destination rejection")
	}
	if err := SaveConfig(path, config); err == nil {
		t.Fatal("expected symlink destination rejection")
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected load symlink rejection")
	}
}
