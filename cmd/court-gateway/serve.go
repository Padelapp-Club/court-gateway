package main

import (
	"context"
	"crypto/tls"
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
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/drivers"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/remote"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
	"github.com/Padelapp-Club/court-gateway/internal/update"
)

// statusInterval is how often serve runs its self-check (CRM login and a
// narrow occupancy request). The one result feeds the health status file
// (and so the container healthcheck) and the heartbeat to the hub; there
// is no other scheduled CRM call. A variable for tests.
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
	if *logFile != "" && stderr == io.Writer(os.Stderr) {
		core.TrimStartupLog(os.Stderr, 1<<20)
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
	clubs, syncer, err := buildInstallation(s, log)
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
			Addr: s.Listen, Handler: clubs.Handler(), ReadHeaderTimeout: 10 * time.Second,
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
	var client *tunnel.Client
	if s.Hub != nil {
		roots, err := tunnel.LoadRoots(s.Hub.CAFile)
		if err != nil {
			fmt.Fprintf(stderr, "court-gateway: %v\n", err)
			return 2
		}
		client, err = tunnel.NewClient(tunnel.ClientConfig{
			HubURL: s.Hub.URL, Secret: s.Hub.Secret, Polls: s.Hub.Polls, Handler: clubs.TunnelHandler(),
			Version: version, Build: build(), Platform: platform(), Logger: log,
			Transport: tunnel.NewTransport(roots), OnConfigVersion: syncer.Notify,
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
		syncer.SetFetch(client.FetchConfig)
		go syncer.Run(ctx)
		go client.Run(ctx)
	}

	// self-check -> health status file (read by `healthcheck`) and heartbeat
	// to the hub: the CRM is checked every statusInterval, the hub
	// connection every 2 seconds; the file is rewritten after each CRM check
	// and whenever the hub connection changes, and each CRM check is sent to
	// the hub as a heartbeat over the gateway's own connection
	go func() {
		t := time.NewTicker(min(2*time.Second, statusInterval))
		defer t.Stop()
		check := &tunnel.SelfCheck{Ready: clubs.Ready, Reason: core.ReadyReason, Interval: statusInterval,
			Timeout: core.ReadyTimeout, Version: version, Build: build()}
		var st core.HealthStatus
		lastHub := ""
		for {
			hubState := ""
			if s.Hub != nil {
				hubState = "disconnected"
				if gw.connected() {
					hubState = "connected"
				}
			}
			hb, due := check.Step(ctx)
			if ctx.Err() != nil {
				return
			}
			if due {
				st = core.HealthStatus{Version: version, Ready: hb.Ready, Reason: hb.Reason}
				gw.ready.Store(st.Ready)
				gw.reason.Store(st.Reason)
				if client != nil {
					cs := syncer.State()
					hb.ConfigVersion, hb.ConfigError, hb.ClubErrors, hb.Clubs = cs.Version, cs.Error, cs.ClubErrors, cs.Clubs
					go sendHeartbeat(ctx, client, hb, log)
				}
			}
			if due || hubState != lastHub {
				st.Time, st.Hub, lastHub = time.Now().UTC(), hubState, hubState
				if werr := core.WriteHealthStatus(s.StateDir, st); werr != nil {
					log.Warn("cannot write the health status file", "error", werr)
				}
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

	log.Info("started", "version", version, "build", build(), "listen", s.Listen, "hub", s.Hub != nil, "crm", s.CRMType, "club", s.ClubID)
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
		var env []string
		if cache, ok := inst.(*update.CacheDir); ok {
			// the next binary must keep using the cache, not replace itself
			env = append(env, update.LaunchedEnv+"=1", update.ShippedEnv+"="+cache.Shipped)
		}
		err := update.Exec(target, os.Args[1:], env...)
		if errors.Is(err, update.ErrRestartRequired) {
			return exitRestart
		}
		log.Error("re-exec failed; the service manager will restart the binary", "error", err)
		return exitRestart
	}
	return code
}

// buildInstallation builds what serve runs: without a hub the club of
// gateway.yaml; with a hub the CRM account, the multi-club handler and the
// syncer that keeps the clubs equal to the installation's config on the
// hub (nil without a hub).
func buildInstallation(s *core.Settings, log *slog.Logger) (*core.Clubs, *remote.Syncer, error) {
	if s.Hub == nil {
		drv, err := core.BuildDriver(s, drivers.Factories)
		if err != nil {
			return nil, nil, err
		}
		clubs, err := core.NewClubs(s, core.ClubsOptions{Logger: log})
		if err != nil {
			return nil, nil, err
		}
		clubs.Set([]*core.Server{core.NewServer(s, drv, log)})
		return clubs, nil, nil
	}
	f, ok := drivers.Accounts[s.CRMType]
	if !ok {
		return nil, nil, &core.ConfigError{Problems: []string{fmt.Sprintf("crm.type: неизвестный тип CRM «%s»", s.CRMType)}}
	}
	acct, err := f(s.Driver)
	if err != nil {
		return nil, nil, &core.ConfigError{Problems: strings.Split(err.Error(), "\n")}
	}
	clubs, err := core.NewClubs(s, core.ClubsOptions{Logger: log, ReadyNoClubs: acct.CheckLogin})
	if err != nil {
		return nil, nil, err
	}
	clubs.SetGatewayAPI(remote.DiscoverHandler(acct, clubs, log))
	return clubs, remote.New(remote.Options{Base: s, Factories: drivers.Factories, Clubs: clubs, Logger: log}), nil
}

// sendHeartbeat reports one self-check to the hub. A failed heartbeat is
// only logged at debug level: the hub notices missing heartbeats itself,
// and a broken connection is already logged by the tunnel client.
func sendHeartbeat(ctx context.Context, c *tunnel.Client, hb tunnel.Heartbeat, log *slog.Logger) {
	hctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := c.SendHeartbeat(hctx, hb); err != nil && ctx.Err() == nil {
		log.Debug("heartbeat not delivered", "error", err)
	}
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
	hubCfg := fs.String("hub", "", "probe the hub running with this hub.yaml instead of a gateway")
	if fs.Parse(args) != nil {
		return 2
	}
	if *hubCfg != "" {
		return probeHub(*hubCfg, stdout, stderr, getenv)
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

// probeHub asks the hub on this machine for /health/ready over loopback.
// The hub's certificate is issued for its public name, not for 127.0.0.1,
// so this local liveness probe does not verify it; it sends no secret.
func probeHub(cfgPath string, stdout, stderr io.Writer, getenv func(string) string) int {
	var c struct {
		Listen string `yaml:"listen"`
		TLS    struct {
			CertFile string `yaml:"cert_file"`
		} `yaml:"tls"`
	}
	data, err := os.ReadFile(cfgPath)
	if err == nil {
		err = yaml.Unmarshal(data, &c)
	}
	if err != nil {
		fmt.Fprintf(stderr, "healthcheck: %v\n", err)
		return 1
	}
	if c.Listen == "" {
		c.Listen = hub.DefaultListen
	}
	_, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		fmt.Fprintf(stderr, "healthcheck: неверный listen %q\n", c.Listen)
		return 1
	}
	scheme := "http"
	tr := &http.Transport{}
	if c.TLS.CertFile != "" {
		scheme = "https"
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // loopback liveness probe, no secret sent
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: tr}
	resp, err := client.Get(scheme + "://127.0.0.1:" + port + "/health/ready")
	if err != nil {
		fmt.Fprintln(stderr, "healthcheck: хаб не отвечает")
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
	clubs, _, err := buildInstallation(s, nil)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := clubs.Ready(ctx); err != nil {
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
