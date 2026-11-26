//go:build windows

package hubauth

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestEnsureAdminTokenRejectsAnUntrustedWindowsACL(t *testing.T) {
	for _, target := range []string{"directory", "token"} {
		t.Run(target, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "data")
			if _, err := EnsureAdminToken(dir, ""); err != nil {
				t.Fatal(err)
			}
			path := dir
			if target == "token" {
				path = filepath.Join(dir, "admin-token")
			}
			descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
			if err != nil {
				t.Fatal(err)
			}
			dacl, _, err := descriptor.DACL()
			if err != nil {
				t.Fatal(err)
			}
			if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := EnsureAdminToken(dir, ""); err == nil {
				t.Fatal("accepted a path accessible by Everyone")
			}
		})
	}
}
