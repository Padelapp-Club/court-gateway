//go:build windows

package update

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"
)

// Handover runs path as a child process (Windows cannot replace a running
// process) and returns its exit code. When ctx ends (the service is
// stopped) the child's stdin is closed, which stops it (see StopOnEOF), and
// it is killed if it is still running a minute later.
func Handover(ctx context.Context, path string, args, env []string) (int, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = append(env, StopOnEOFEnv+"=1")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			stdin.Close()
			select {
			case <-done:
			case <-time.After(time.Minute):
				_ = cmd.Process.Kill()
			}
		case <-done:
		}
	}()
	err = cmd.Wait()
	close(done)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 0, err
}

// Restart returns ErrRestartRequired on Windows.
func Restart(string, []string) error { return ErrRestartRequired }
