//go:build !windows

package update

import (
	"os"
	"syscall"
)

// ErrRestartRequired is returned by Exec where a process cannot replace
// itself; the caller exits and the service manager starts the new binary.
var ErrRestartRequired = errorString("restart required to run the new binary")

type errorString string

func (e errorString) Error() string { return string(e) }

// Exec replaces the current process with path, keeping the arguments and
// the environment plus extra variables.
func Exec(path string, args []string, extraEnv ...string) error {
	argv := append([]string{path}, args...)
	return syscall.Exec(path, argv, append(os.Environ(), extraEnv...))
}
