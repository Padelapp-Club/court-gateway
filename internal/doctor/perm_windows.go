//go:build windows

package doctor

import (
	"os"
	"path/filepath"

	"github.com/Padelapp-Club/court-gateway/internal/core"
)

func secretsFileCheckOS(cfgPath string) Check {
	p := filepath.Join(filepath.Dir(cfgPath), core.EnvFileName)
	if _, err := os.Stat(p); err != nil {
		return Check{Code: CodeSecretFile, Status: Skip, Message: "файла .env нет: секреты берутся из окружения"}
	}
	// Windows ACLs are not inspected; the install instructions put the
	// folder under the service account's control.
	return Check{Code: CodeSecretFile, Status: OK, Message: ".env найден"}
}
