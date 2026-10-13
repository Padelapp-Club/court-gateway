//go:build !windows

package doctor

import (
	"os"
	"path/filepath"

	"github.com/Padelapp-Club/court-gateway/internal/core"
)

func secretsFileCheck(cfgPath string) Check {
	p := filepath.Join(filepath.Dir(cfgPath), core.EnvFileName)
	info, err := os.Stat(p)
	if err != nil {
		return Check{Code: CodeSecretFile, Status: Skip, Message: "файла .env нет: секреты берутся из окружения"}
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Check{Code: CodeSecretFile, Status: Warn, Message: ".env доступен другим пользователям компьютера",
			Hint: "chmod 600 " + p}
	}
	return Check{Code: CodeSecretFile, Status: OK, Message: ".env доступен только владельцу"}
}
