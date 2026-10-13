//go:build !windows

package update

import (
	"context"
	"syscall"
)

// Handover replaces the current process with path (the same pid for the
// service manager). It returns only when that fails.
func Handover(_ context.Context, path string, args, env []string) (int, error) {
	return 0, syscall.Exec(path, append([]string{path}, args...), env)
}

// Restart starts the installed binary again in place of this process; it
// then runs the version that is current. It returns only when that fails.
func Restart(installed string, args []string) error {
	return syscall.Exec(installed, append([]string{installed}, args...), RestartEnv())
}
