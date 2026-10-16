//go:build windows

package agent

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"golang.org/x/sys/windows"
)

// openPrivateFile opens path without following reparse points and checks
// the opened handle: a regular file owned by the current user whose DACL
// grants access only to that user and SYSTEM. Inspection failures and
// unsupported ACE forms are refusals. These are operational guardrails,
// not a boundary against a hostile process running as the same user.
func openPrivateFile(path string) (*os.File, error) {
	return openPrivate(path, windows.GENERIC_READ, windows.OPEN_EXISTING)
}

func openPrivateCreate(path string) (*os.File, error) {
	f, err := openPrivate(path, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.OPEN_ALWAYS)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// createPrivateExclusive creates a new file with an owner and SYSTEM only
// access list, failing if anything already exists at path.
func createPrivateExclusive(path string) (*os.File, error) {
	return openPrivate(path, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.CREATE_NEW)
}

func openPrivate(path string, access uint32, disposition uint32) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	var sa *windows.SecurityAttributes
	if disposition != windows.OPEN_EXISTING {
		sd, err := privateDescriptor()
		if err != nil {
			return nil, err
		}
		sa = &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	}
	h, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, sa, disposition, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, fmt.Errorf("cannot open: %w", err)
	}
	f := os.NewFile(uintptr(h), path)
	if err := checkPrivateHandle(h); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func privateDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	sid, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString(fmt.Sprintf("O:%sG:%sD:P(A;;GA;;;%s)(A;;GA;;;SY)", sid, sid, sid))
}

func currentUserSID() (*windows.SID, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid.Copy()
}

func checkPrivateHandle(h windows.Handle) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return fmt.Errorf("cannot inspect: %w", err)
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		return fmt.Errorf("must be a regular file, not a directory or reparse point")
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("cannot inspect permissions: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return fmt.Errorf("cannot inspect owner")
	}
	me, err := currentUserSID()
	if err != nil {
		return err
	}
	if !owner.Equals(me) {
		return fmt.Errorf("must be owned by the current user")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return fmt.Errorf("must have an owner-only access list (no NULL DACL)")
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return fmt.Errorf("cannot inspect access list")
		}
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if !sid.Equals(me) && !sid.Equals(system) {
				return fmt.Errorf("must grant access only to the current user and SYSTEM")
			}
		case windows.ACCESS_DENIED_ACE_TYPE:
			// A deny entry only narrows access.
		default:
			return fmt.Errorf("unsupported access list entry")
		}
	}
	return nil
}

func lockStateFile(f *os.File) error {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return storage.ErrLocked
	}
	return err
}
