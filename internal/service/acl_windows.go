//go:build windows

package service

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ProtectSecrets replaces the DACL of path (the folder holding .env, or
// .env) with a protected one of exactly two entries, full control for
// Administrators and SYSTEM (inherited by a folder's contents), then reads
// it back and checks it with ACLProblem. Any explicit entry the path had
// is gone. An error means the path is not known to be closed.
func ProtectSecrets(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	var inherit uint32
	if fi.IsDir() {
		inherit = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	}
	var entries []windows.EXPLICIT_ACCESS
	for _, s := range []string{SIDAdministrators, SIDSystem} {
		sid, err := windows.StringToSid(s)
		if err != nil {
			return err
		}
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.SET_ACCESS,
			Inheritance:       inherit,
			Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(sid)},
		})
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
	if err != nil {
		return err
	}
	protected, null, aces, err := ReadACL(path)
	if err != nil {
		return err
	}
	if p := ACLProblem(protected, null, aces); p != "" {
		return errors.New(p)
	}
	return nil
}

// ReadACL reads path's DACL for ACLProblem.
func ReadACL(path string) (protected, nullDACL bool, aces []ACE, err error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, false, nil, err
	}
	control, _, err := sd.Control()
	if err != nil {
		return false, false, nil, err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return false, false, nil, err
	}
	if dacl == nil {
		return control&windows.SE_DACL_PROTECTED != 0, true, nil, nil
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false, false, nil, err
		}
		aces = append(aces, ACE{Type: ace.Header.AceType, SID: (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()})
	}
	return control&windows.SE_DACL_PROTECTED != 0, false, aces, nil
}

// secretFileSDDL is the DACL of ProtectSecrets for a file: protected, full
// control for Administrators (BA) and SYSTEM (SY), nothing else.
const secretFileSDDL = "D:P(A;;FA;;;BA)(A;;FA;;;SY)"

// CreateSecretFile makes the new file path, which must not exist, with
// the DACL ProtectSecrets sets, given at its creation: no other user can
// open it at any moment, and nobody while it is being written (no
// sharing). It returns the file open for writing.
func CreateSecretFile(path string) (*os.File, error) {
	sd, err := windows.SecurityDescriptorFromString(secretFileSDDL)
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	h, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "create", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
