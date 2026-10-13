//go:build !windows

package doctor

import "os"

// secretsExposure: .env must be readable by its owner only.
func secretsExposure(env string) (Status, string, string) {
	info, err := os.Stat(env)
	if err == nil && info.Mode().Perm()&0o077 != 0 {
		return Warn, ".env доступен другим пользователям компьютера", "chmod 600 " + env
	}
	return OK, ".env доступен только владельцу", ""
}
