//go:build windows

package hub

import (
	"io/fs"
	"os"

	"github.com/Padelapp-Club/court-gateway/internal/service"
)

// createKeyFile makes the new file p for a signing key closed to everyone
// but Administrators and SYSTEM from its creation, as setup closes .env
// (service.ProtectSecrets): the hub runs as SYSTEM or from an elevated
// prompt.
func createKeyFile(p string) (*os.File, error) { return service.CreateSecretFile(p) }

// keyFileExposure says why others than Administrators and SYSTEM may read
// the signing key file p, "" when they may not (service.ACLProblem).
func keyFileExposure(p string, _ fs.FileInfo) string {
	protected, null, aces, err := service.ReadACL(p)
	if err != nil {
		return "права не читаются: " + err.Error()
	}
	if msg := service.ACLProblem(protected, null, aces); msg != "" {
		return msg + ` — оставьте доступ только администраторам и системе: icacls "` + p + `" /inheritance:r /grant:r *S-1-5-32-544:F *S-1-5-18:F (и удалите прочие записи)`
	}
	return ""
}
