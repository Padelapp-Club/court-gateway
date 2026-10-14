//go:build windows

package doctor

import (
	"path/filepath"

	"github.com/Padelapp-Club/court-gateway/internal/service"
)

// secretsExposure: the folder holding .env and .env itself must be closed
// to everyone but Administrators and SYSTEM (service.ProtectSecrets, which
// setup and install-service apply and verify).
func secretsExposure(cfgPath, env string) (Status, string, string) {
	hint := `court-gateway install-service -config "` + cfgPath + `" (PowerShell от имени администратора; если служба уже установлена — сначала court-gateway uninstall-service)`
	for _, p := range []string{filepath.Dir(env), env} {
		protected, null, aces, err := service.ReadACL(p)
		if err != nil {
			return Fail, "права " + p + " не читаются: " + err.Error(), hint
		}
		if msg := service.ACLProblem(protected, null, aces); msg != "" {
			return Fail, p + ": " + msg, hint
		}
	}
	return OK, "папка с .env и сам .env закрыты: доступ только администраторам и системе", ""
}
