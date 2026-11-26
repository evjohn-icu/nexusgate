//go:build windows

// Package winprivate protects Hub credential files with Windows access control.
package winprivate

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

var trusted = sync.OnceValues(func() ([]*windows.SID, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, err
	}
	if windows.EqualSid(user.User.Sid, system) {
		return []*windows.SID{system, admins}, nil
	}
	return []*windows.SID{user.User.Sid, system, admins}, nil
})

func isTrusted(sid *windows.SID, trustees []*windows.SID) bool {
	for _, trustee := range trustees {
		if sid != nil && windows.EqualSid(sid, trustee) {
			return true
		}
	}
	return false
}

func attributes(aceFlags string) (*windows.SecurityAttributes, error) {
	trustees, err := trusted()
	if err != nil {
		return nil, err
	}
	sddl := "D:P"
	for _, sid := range trustees {
		sddl += "(A;" + aceFlags + ";FA;;;" + sid.String() + ")"
	}
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, err
	}
	attrs := &windows.SecurityAttributes{SecurityDescriptor: descriptor}
	attrs.Length = uint32(unsafe.Sizeof(*attrs))
	return attrs, nil
}

// CheckType rejects reparse points, including directory junctions.
func CheckType(path string, directory bool) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attrs, err := windows.GetFileAttributes(name)
	if err != nil {
		return err
	}
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory {
		return errors.New("unexpected private file type or reparse point")
	}
	return nil
}

// Validate requires a file or directory to be owned and controlled by the
// running account, LocalSystem or Builtin Administrators.
func Validate(path string, directory bool) error {
	if err := CheckType(path, directory); err != nil {
		return err
	}
	trustees, err := trusted()
	if err != nil {
		return err
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	if !isTrusted(owner, trustees) {
		return errors.New("private path has an untrusted owner")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return errors.New("private path requires an access control list")
	}
	const fileDeleteChild = 0x0040 // FILE_DELETE_CHILD
	const sensitive = windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | fileDeleteChild |
		windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_READ | windows.GENERIC_WRITE | windows.GENERIC_ALL
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			if ace.Mask&sensitive == 0 {
				continue
			}
			if isTrusted((*windows.SID)(unsafe.Pointer(&ace.SidStart)), trustees) {
				continue
			}
		default:
			return errors.New("private path has an unsupported access rule")
		}
		return errors.New("private path grants access to an untrusted account")
	}
	return nil
}

// EnsureDirectory creates the final directory with a protected inheritable
// DACL. An existing directory is checked without changing its permissions.
func EnsureDirectory(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return Validate(path, true)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attrs, err := attributes("OICI")
	if err != nil {
		return err
	}
	if err := windows.CreateDirectory(name, attrs); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return fmt.Errorf("create private directory: %w", err)
	}
	return Validate(path, true)
}

// CreateTemp protects the file before any plaintext can be written.
func CreateTemp(dir, prefix string) (*os.File, error) {
	attrs, err := attributes("")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, prefix+idgen.New())
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, attrs, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

// Replace publishes a previously flushed sibling file with write-through.
func Replace(source, destination string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
