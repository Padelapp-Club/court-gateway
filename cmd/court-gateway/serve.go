package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
	"github.com/Padelapp-Club/court-gateway/internal/update"
)

// statusInterval is how often serve checks readiness for the health
// status file (and so for the container healthcheck). A variable for tests.
var statusInterval = 60 * time.Second

// confirmDeadline is how long a freshly updated binary has to become
// ready (and connected to the hub) before it rolls itself back.
const confirmDeadline = 3 * time.Minute

func cmdServe(args []string, stderr io.Writer, getenv func(string) string, defaultConfig string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfig, "config file")
	logFile := fs.String("log-file", "", "write the log to this file (rotated at 10 MB) instead of stderr")
	if fs.Parse(args) != nil {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, *cfgPath, *logFile, stderr, getenv)
}

type gateway struct {
	ready     atomic.Bool
	reason    atomic.Value // string
	connected func() bool
}

func serve(ctx context.Context, cfgPath, logFile string, stderr io.Writer, getenv func(string) string) int {
	getenv, err := core.EnvWithDotEnv(cfgPath, getenv)
	if err != nil {
		fmt.Fprintf(stderr, "court-gateway: %v\n", err)
		return 2
	}
	s, err := core.LoadFile(cfgPath, getenv)
	if err != nil {
		fmt.Fprintf(stderr, "court-gateway: %v\n", err)
		return 2
	}
	var out io.Writer = stderr
	if logFile != "" {
		rf := &core.RotatingFile{Path: logFile, Max: 10 << 20}
		defer rf.Close()
		out = rf
	}
	log := core.NewLogger(out, slog.LevelInfo, s.Secrets)

	// A container ships a read-only binary; updates live in the state
	// directory, and the shipped binary hands over to the newest verified
	// one here, before doing anything else.
	exe, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	inst, canUpdate := update.ChooseInstaller(exe, s.StateDir)
	if cache, ok := inst.(*update.CacheDir); ok {
		if next := cache.Launch(version, time.Now()); next != "" {
			log.Info("starting the updated binary", "path", next)
			if err := update.Exec(next, os.Args[1:], update.LaunchedEnv+"=1", update.ShippedEnv+"="+cache.Shipped); err != nil {
				log.Error("could not start the updated binary, staying on the shipped one", "error", err)
			}
		}
	}

	s.Driver.Logger = log
	drv, err := core.BuildDriver(s, factories)
	if err != nil {
		fmt.Fprintf(stderr, "court-gateway: %v\n", err)
		return 2
	}
	srv, err := core.NewServer(s, drv, log)
	if err != nil {
		fmt.Fprintf(stderr, "court-gateway: %v\n", err)
		return 2
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	restartTo := make(chan string, 1)
	gw := &gateway{connected: func() bool { return true }}
	gw.reason.Store("STARTING")

	// local port: only when asked for (LAN, debugging, or no hub)
	var httpSrv *http.Server
	errc := make(chan error, 1)
	if s.Listen != "" {
		httpSrv = &http.Server{
			Addr: s.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second,
			MaxHeaderBytes: 64 << 10,
		}
		ln, err := net.Listen("tcp", s.Listen)
		if err != nil {
			log.Error("cannot open the local port", "listen", s.Listen, "error", err)
			return 1
		}
		go func() { errc <- httpSrv.Serve(ln) }()
	}

	// the outbound tunnel to the hub
	hint := make(chan struct{}, 1)
	if s.Hub != nil {
		roots, err := tunnel.LoadRoots(s.Hub.CAFile)
		if err != nil {
			fmt.Fprintf(stderr, "court-gateway: %v\n", err)
			return 2
		}
		client, err := tunnel.NewClient(tunnel.ClientConfig{
			HubURL: s.Hub.URL, Secret: s.Hub.Secret, Polls: s.Hub.Polls, Handler: srv.TunnelHandler(),
			Version: version, Build: build(), Platform: platform(), Logger: log,
			Transport: tunnel.NewTransport(roots),
			OnHint: func(string) {
				select {
				case hint <- struct{}{}:
				default:
				}
			},
		})
		if err != nil {
			fmt.Fprintf(stderr, "court-gateway: %v\n", err)
			return 2
		}
		gw.connected = client.Connected
		go client.Run(ctx)
	}

	// readiness -> health status file (read by `healthcheck`)
	go func() {
		t := time.NewTicker(statusInterval)
		defer t.Stop()
		for {
			rctx, rcancel := context.WithTimeout(ctx, core.ReadyTimeout)
			err := drv.Ready(rctx)
			rcancel()
			if ctx.Err() != nil {
				return
			}
			st := core.HealthStatus{Time: time.Now().UTC(), Version: version, Ready: err == nil}
			if err != nil {
				st.Reason = core.ReadyReason(err)
			}
			gw.ready.Store(st.Ready)
			gw.reason.Store(st.Reason)
			if s.Hub != nil {
				st.Hub = "disconnected"
				if gw.connected() {
					st.Hub = "connected"
				}
			}
			if werr := core.WriteHealthStatus(s.StateDir, st); werr != nil {
				log.Warn("cannot write the health status file", "error", werr)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	// a freshly updated binary confirms itself or rolls back
	if canUpdate {
		go func() {
			target, err := update.Confirm(ctx, s.StateDir, version, inst, func(ctx context.Context) error {
				return waitHealthy(ctx, gw, s.Hub != nil, confirmDeadline)
			}, log, nil)
			if err != nil && ctx.Err() == nil {
				log.Error("update confirmation", "error", err)
			}
			if target != "" {
				restartTo <- target
			}
		}()
	}

	// automatic updates
	key, kerr := update.ParsePublicKey(releaseKey)
	switch {
	case kerr != nil:
		log.Error("embedded release key is invalid; automatic updates are off")
	case key == nil:
		log.Info("self-built binary: automatic updates are off", "version", version)
	case !s.Update.Enabled:
		log.Info("automatic updates are off in the config", "version", version)
	case !canUpdate:
		log.Warn("the binary's folder is not writable; automatic updates are off", "path", exe)
	default:
		u := &update.Updater{
			PublicKey: key, ManifestURL: s.Update.ManifestURL, Current: version, Client: &http.Client{Timeout: 5 * time.Minute},
			StateDir: s.StateDir, Installer: inst, CheckBinary: update.ChildCheck(cfgPath), Log: log,
		}
		first := 5*time.Minute + time.Duration(time.Now().UnixNano()%int64(10*time.Minute))
		go u.Loop(ctx, hint, first, 24*time.Hour, func(res update.Result) { restartTo <- res.Target })
	}

	log.Info("started", "version", version, "build", build(), "listen", s.Listen, "hub", s.Hub != nil, "crm", s.CRMType, "courts", len(s.Courts))
	code := 0
	var target string
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("server stopped", "error", err)
			code = 1
		}
	case <-ctx.Done():
		log.Info("shutting down")
	case target = <-restartTo:
		log.Info("restarting into another binary", "path", target)
	}
	cancel()
	if httpSrv != nil {
		shCtx, shCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = httpSrv.Shutdown(shCtx)
		shCancel()
	}
	if target != "" {
		err := update.Exec(target, os.Args[1:])
		if errors.Is(err, update.ErrRestartRequired) {
			return exitRestart
		}
		log.Error("re-exec failed; the service manager will restart the binary", "error", err)
		return exitRestart
	}
	return code
}

// waitHealthy waits until the gateway is ready and, with a hub, connected.
func waitHealthy(ctx context.Context, gw *gateway, hub bool, deadline time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		if gw.ready.Load() && (!hub || gw.connected()) {
			return nil
		}
		select {
		case <-ctx.Done():
			reason, _ := gw.reason.Load().(string)
			if hub && !gw.connected() {
				reason += " HUB_DISCONNECTED"
			}
			return fmt.Errorf("not healthy within %s: %s", deadline, reason)
		case <-t.C:
		}
	}
}

func cmdHealthcheck(args []string, stdout, stderr io.Writer, getenv func(string) string, defaultConfig string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfig, "config file")
	addr := fs.String("addr", "", "host:port of the running service (overrides the config)")
	if fs.Parse(args) != nil {
		return 2
	}
	if *addr != "" {
		return probeHTTP(*addr, stdout, stderr)
	}
	stateDir, listen, hub := core.StatePath(*cfgPath, getenv)
	if !hub && listen != "" {
		return probeHTTP(listen, stdout, stderr)
	}
	return probeStatusFile(stateDir, stdout, stderr)
}

// probeStatusFile reads what the running service last observed. It needs
// no secrets and opens no connection.
func probeStatusFile(dir string, stdout, stderr io.Writer) int {
	st, err := core.ReadHealthStatus(dir)
	if err != nil {
		fmt.Fprintln(stderr, "healthcheck: служба не запущена или ещё не проверила готовность")
		return 1
	}
	age := time.Since(st.Time)
	fmt.Fprintf(stdout, "ready=%v reason=%s hub=%s age=%s version=%s\n", st.Ready, st.Reason, st.Hub, age.Round(time.Second), st.Version)
	if age > 3*statusInterval {
		fmt.Fprintln(stderr, "healthcheck: служба давно не обновляла статус — зависла или остановлена")
		return 1
	}
	if !st.Ready {
		return 1
	}
	return 0
}

// probeHTTP asks a running service for /health/ready on its local port.
func probeHTTP(addr string, stdout, stderr io.Writer) int {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintf(stderr, "healthcheck: неверный адрес %q\n", addr)
		return 1
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: core.ReadyTimeout + 2*time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/health/ready")
	if err != nil {
		fmt.Fprintln(stderr, "healthcheck: сервис не отвечает")
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	fmt.Fprintf(stdout, "%d %s", resp.StatusCode, body)
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// cmdSelfcheck is what a running gateway asks a freshly downloaded binary
// to do before switching to it: load the live config, log in to the CRM,
// reach the hub. Exit 0 ok; update.SelfCheckTransientExit when the
// environment failed; 1 when the binary cannot work with this config.
func cmdSelfcheck(args []string, stdout, stderr io.Writer, getenv func(string) string, defaultConfig string) int {
	fs := flag.NewFlagSet("selfcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfig, "config file")
	if fs.Parse(args) != nil {
		return 2
	}
	getenv, err := core.EnvWithDotEnv(*cfgPath, getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	s, err := core.LoadFile(*cfgPath, getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	drv, err := core.BuildDriver(s, factories)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if _, err := core.NewServer(s, drv, nil); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := drv.Ready(ctx); err != nil {
		fmt.Fprintln(stderr, "CRM: "+core.ReadyReason(err))
		return update.SelfCheckTransientExit
	}
	if s.Hub != nil {
		roots, err := tunnel.LoadRoots(s.Hub.CAFile)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if _, err := tunnel.FetchWhoami(ctx, tunnel.NewTransport(roots), s.Hub.URL, s.Hub.Secret); err != nil {
			fmt.Fprintln(stderr, "hub: unreachable or secret rejected")
			return update.SelfCheckTransientExit
		}
	}
	fmt.Fprintln(stdout, "ok")
	return 0
}
