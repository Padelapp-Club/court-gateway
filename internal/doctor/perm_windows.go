//go:build windows

package doctor

import (
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Padelapp-Club/court-gateway/internal/service"
)

// secretsExposure: the folder holding .env must be closed to everyone but
// Administrators and SYSTEM (WindowsRestrictCommand, which setup and
// install-service apply).
func secretsExposure(env string) (Status, string, string) {
	dir := filepath.Dir(env)
	hint := strings.Join(service.WindowsRestrictCommand(dir), " ") + " (PowerShell от имени администратора)"
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return Fail, "права папки " + dir + " не читаются: " + err.Error(), hint
	}
	control, _, err := sd.Control()
	if err != nil {
		return Fail, "права папки " + dir + " не читаются: " + err.Error(), hint
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return Fail, "права папки " + dir + " не читаются: " + err.Error(), hint
	}
	var allowed []string
	if dacl != nil {
		for i := uint32(0); i < uint32(dacl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, i, &ace); err != nil {
				return Fail, "права папки " + dir + " не читаются: " + err.Error(), hint
			}
			if ace.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE {
				allowed = append(allowed, (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String())
			}
		}
	}
	if p := folderACLProblem(control&windows.SE_DACL_PROTECTED != 0, dacl == nil, allowed); p != "" {
		return Fail, p, hint
	}
	return OK, "папка с .env закрыта: доступ только администраторам и системе", ""
}
