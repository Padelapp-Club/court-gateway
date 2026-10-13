//go:build windows

package update

// ErrRestartRequired is returned by Exec where a process cannot replace
// itself; the caller exits and the service manager (the Windows service
// recovery actions set by install-service) starts the new binary.
var ErrRestartRequired = errorString("restart required to run the new binary")

type errorString string

func (e errorString) Error() string { return string(e) }

// Exec cannot replace a process on Windows; the new binary is already at
// the same path, so a restart by the service manager runs it.
func Exec(string, []string, ...string) error { return ErrRestartRequired }
