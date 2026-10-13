package update

import (
	"errors"
	"io"
	"os"
)

// ErrRestartRequired is returned by Restart where a process cannot replace
// itself (Windows). The caller exits with its restart code: the installed
// binary that started it (Handover) or the service manager starts the
// version that is current.
var ErrRestartRequired = errors.New("restart required to run another version")

// StopOnEOFEnv is set on a version started by Handover on Windows: the
// version stops when its stdin closes, which is how the installed binary
// passes on a stop of the service.
const StopOnEOFEnv = "COURT_GATEWAY_STOP_ON_EOF"

// StopOnEOF calls stop when stdin closes, if this process was started by
// Handover on Windows; otherwise it does nothing.
func StopOnEOF(stop func()) {
	if os.Getenv(StopOnEOFEnv) == "" {
		return
	}
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		stop()
	}()
}
