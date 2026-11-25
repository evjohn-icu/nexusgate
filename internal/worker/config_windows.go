//go:build windows

package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"github.com/evjohn-icu/nexusgate/internal/idgen"
	"golang.org/x/sys/windows"
)

// Windows cannot express the Unix mode checks this package uses on other
// platforms, so the Worker config is protected with an explicit DACL instead:
// full control for the running account, LocalSystem and Builtin Administrators
// and no inherited ACEs from the containing directories. Files whose DACL lets
// other accounts read or write the token are rejected without touching their
// contents, so an operator can move or repair the file instead of losing it.

// configSensitiveAccess is every file right that would let an account that is
// not the running account, LocalSystem or Administrators read or write the
// config, or take control of its protection.
const configSensitiveAccess = windows.ACCESS_MASK(windows.FILE_READ_DATA |
	windows.FILE_READ_EA | windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL |
	windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA |
	windows.FILE_WRITE_ATTRIBUTES | windows.DELETE | windows.WRITE_DAC |
	windows.WRITE_OWNER | windows.GENERIC_READ | windows.GENERIC_WRITE | windows.GENERIC_ALL)

var (
	runningSIDOnce  sync.Once
	runningSIDValue *windows.SID
	runningSIDError error
)

// runningAccountSID resolves the SID of the account this Worker process runs
// as. It is cached because the identity cannot change within a process.
func runningAccountSID() (*windows.SID, error) {
	runningSIDOnce.Do(func() {
		token, err := windows.OpenCurrentProcessToken()
		if err != nil {
			runningSIDError = err
			return
		}
		defer token.Close()
		user, err := token.GetTokenUser()
		if err != nil {
			runningSIDError = err
			return
		}
		runningSIDValue = user.User.Sid
	})
	if runningSIDError != nil {
		return nil, runningSIDError
	}
	return runningSIDValue, nil
}

// isConfigPrincipal reports whether sid is one of the accounts a Worker config
// may be owned by or grant access to.
func isConfigPrincipal(sid *windows.SID) bool {
	if sid == nil {
		return false
	}
	running, err := runningAccountSID()
	if err != nil {
		return false
	}
	if windows.EqualSid(sid, running) {
		return true
	}
	if system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid); err == nil && windows.EqualSid(sid, system) {
		return true
	}
	if admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid); err == nil && windows.EqualSid(sid, admins) {
		return true
	}
	return false
}

// protectedConfigDACL builds the DACL every Worker config file and newly
// created config parent carries: full access for the running account,
// LocalSystem and Builtin Administrators only.
func protectedConfigDACL() (*windows.SecurityAttributes, error) {
	running, err := runningAccountSID()
	if err != nil {
		return nil, fmt.Errorf("resolve worker identity: %w", err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, fmt.Errorf("resolve LocalSystem SID: %w", err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, fmt.Errorf("resolve Administrators SID: %w", err)
	}
	trustees := make(map[string]bool, 3)
	var sddl string
	for _, sid := range []*windows.SID{running, system, admins} {
		name := sid.String()
		if trustees[name] {
			continue
		}
		trustees[name] = true
		sddl += "(A;OICI;FA;;;" + name + ")"
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P" + sddl)
	if err != nil {
		return nil, fmt.Errorf("build worker config DACL: %w", err)
	}
	attributes := &windows.SecurityAttributes{SecurityDescriptor: descriptor}
	attributes.Length = uint32(unsafe.Sizeof(*attributes))
	return attributes, nil
}

// isReparsePoint reports whether path carries a Windows reparse point. Go's
// Lstat already flags symlinks and junctions; non-link reparse tags such as
// cloud placeholder files are caught here, so a destination that is not a
// plain file can never be read through.
func isReparsePoint(path string) (bool, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	attributes, err := windows.GetFileAttributes(pointer)
	if err != nil {
		return false, err
	}
	return attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, nil
}

// prepareConfigParent creates the missing ancestors of the Worker config with
// the protected DACL and validates existing ones. An existing directory is
// permitted regardless of its own DACL — unlike the config file, the directory
// holds no secret, and the child config DACL is protected when it is written.
func prepareConfigParent(dir string) error {
	info, err := os.Lstat(dir)
	if err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("worker config parent must be a real directory")
		}
		reparse, err := isReparsePoint(dir)
		if err != nil {
			return fmt.Errorf("inspect worker config parent: %w", err)
		}
		if reparse {
			return errors.New("worker config parent must be a real directory")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect worker config parent: %w", err)
	}
	parent := filepath.Dir(dir)
	if parent == dir {
		return errors.New("worker config parent could not be created")
	}
	if err := prepareConfigParent(parent); err != nil {
		return err
	}
	attributes, err := protectedConfigDACL()
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	if err := windows.CreateDirectory(name, attributes); err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return prepareConfigParent(dir)
		}
		return fmt.Errorf("create private worker config parent: %w", err)
	}
	return nil
}

// validateConfigSecurity checks that an existing config file is owned by an
// allowed account and has no ACE granting another account the ability to
// read, write or re-protect the file.
func validateConfigSecurity(path string) error {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("inspect worker config: %w", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return fmt.Errorf("inspect worker config owner: %w", err)
	}
	if !isConfigPrincipal(owner) {
		return errors.New("worker config must be owned by the running account, LocalSystem or Administrators")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return fmt.Errorf("inspect worker config DACL: %w", err)
	}
	if dacl == nil {
		return errors.New("worker config DACL must be present")
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return fmt.Errorf("inspect worker config DACL: %w", err)
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		default:
			return errors.New("worker config DACL contains an unsupported access rule")
		}
		if ace.Mask&configSensitiveAccess == 0 || ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		trustee := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if isConfigPrincipal(trustee) {
			continue
		}
		return errors.New("worker config must grant access only to the running account, LocalSystem and Administrators")
	}
	return nil
}

// validateConfigFile validates an existing destination and tolerates a missing
// file, so enrollment can check the destination before contacting the Hub.
func validateConfigFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect worker config: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("worker config must be a regular file")
	}
	reparse, err := isReparsePoint(path)
	if err != nil {
		return fmt.Errorf("inspect worker config: %w", err)
	}
	if reparse {
		return errors.New("worker config must be a regular file")
	}
	return validateConfigSecurity(path)
}

// requireConfigFile validates an existing destination and fails when it is
// missing, as loading or verifying a written config must.
func requireConfigFile(path string) error {
	if _, err := os.Lstat(path); err != nil {
		return err
	}
	return validateConfigFile(path)
}

// createConfigTempFile creates the next publication with an explicit DACL so
// its contents and access rules are protected for the life of the file.
func createConfigTempFile(dir string) (*os.File, error) {
	attributes, err := protectedConfigDACL()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, ".worker.json-"+idgen.New())
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, fmt.Errorf("create private worker config: %w", err)
	}
	return os.NewFile(uintptr(handle), path), nil
}

// commitConfigFile publishes the flushed temporary file atomically.
// MOVEFILE_WRITE_THROUGH flushes the rename itself, after the temporary file's
// FlushFileBuffers in SaveConfig.
func commitConfigFile(temporaryPath, finalPath string) error {
	from, err := windows.UTF16PtrFromString(temporaryPath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(finalPath)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// syncConfigDirectory is a no-op on Windows: MoveFileEx with
// MOVEFILE_WRITE_THROUGH already flushes the directory change to disk.
func syncConfigDirectory(dir string) error {
	return nil
}
