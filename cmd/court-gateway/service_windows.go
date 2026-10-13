//go:build windows

package main

import (
	"context"
	"flag"
	"io"
	"path/filepath"

	"golang.org/x/sys/windows/svc"

	"github.com/Padelapp-Club/court-gateway/internal/service"
)

// protectSecrets closes the install folder and .env (setup, install-service).
var protectSecrets = service.ProtectSecrets

// cmdService runs the gateway under the Windows Service Control Manager.
// A Windows service must answer the SCM's control protocol, which only
// golang.org/x/sys/windows/svc implements; the exit code matters because
// install-service sets recovery actions that restart the service after
// any exit — that is how an installed update starts on Windows.
func cmdService(args []string, stderr io.Writer, getenv func(string) string, defaultConfig string) int {
	inService, err := svc.IsWindowsService()
	if err != nil || !inService {
		return cmdServe(args, stderr, getenv, defaultConfig)
	}
	fs := flag.NewFlagSet("service", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgPath := fs.String("config", defaultConfig, "config file")
	if fs.Parse(args) != nil {
		return 2
	}
	h := &winService{cfg: *cfgPath, getenv: getenv, stderr: stderr}
	if err := svc.Run("court-gateway", h); err != nil {
		return 1
	}
	return h.code
}

type winService struct {
	cfg    string
	getenv func(string) string
	stderr io.Writer
	code   int
}

func (w *winService) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	logFile := filepath.Join(filepath.Dir(w.cfg), "court-gateway.log")
	go func() { done <- serve(ctx, w.cfg, logFile, w.stderr, w.getenv) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				w.code = <-done
				return false, 0
			}
		case code := <-done:
			cancel()
			w.code = code
			// a non-zero exit makes the SCM apply the recovery actions
			return true, uint32(code)
		}
	}
}
