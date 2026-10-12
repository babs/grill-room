package daemon

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// restrictToOwner replaces path's DACL with one entry, full access for the current user, and
// stops inheritance: Go's file modes do not apply on Windows, where a new file inherits its
// folder's entries (SYSTEM, Administrators, sometimes others).
func restrictToOwner(path string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("current user: %w", err)
	}
	sd, err := windows.SecurityDescriptorFromString(ownerOnlySDDL(user.User.Sid.String()))
	if err != nil {
		return fmt.Errorf("owner-only DACL: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("owner-only DACL: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("restrict %s to its owner: %w", path, err)
	}
	return nil
}

// ownerOnlySDDL is a protected DACL granting all file rights to sid alone.
func ownerOnlySDDL(sid string) string { return "D:P(A;;FA;;;" + sid + ")" }
