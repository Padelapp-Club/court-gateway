//go:build !windows

package hub

import (
	"fmt"
	"io/fs"
	"os"
)

// createKeyFile makes the new file p for a signing key, readable by its
// owner only.
func createKeyFile(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

// keyFileExposure says why others than the hub's user may read the
// signing key file p, "" when they may not: as .env, it is readable by its
// owner only.
func keyFileExposure(p string, fi fs.FileInfo) string {
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Sprintf("доступен группе или другим пользователям (%s) — chmod 600 %s", fi.Mode().Perm(), p)
	}
	return ""
}
