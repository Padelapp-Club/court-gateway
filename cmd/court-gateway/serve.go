package main

import (
	"cmp"
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
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

// restartProcess starts the installed binary again in place of this
// process (update.Restart); a variable for tests.
var restartProcess = update.Restart

// maxHandovers bounds how often one start of the installed binary hands
// over in a row (Windows: a version that exits to be restarted).
const maxHandovers = 10

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

func serve(ctx context.Context, cfgPath, logFile string, stderr io.Writer, procEnv func(string) string) int {
	getenv, err := core.EnvWithDotEnv(cfgPath, procEnv)
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
	closeLog := func() {}
	if logFile != "" {
		rf := &core.RotatingFile{Path: logFile, Max: 10 << 20}
		defer rf.Close()
		out, closeLog = rf, func() { _ = rf.Close() }
	}
	log := core.NewLogger(out, slog.LevelInfo, s.Secrets)

	// The installed binary is never replaced: updates live in the state
	// directory, and the installed binary hands over to the newest verified
	// one here, before doing anything else.
	installed := update.InstalledPath()
	vers := &update.Versions{Dir: s.StateDir}
	if os.Getenv(update.LaunchedEnv) == "" {
		next := func() string { return vers.Next(version, time.Now(), log) }
		start := func(ctx context.Context, path string) (int, error) {
			return update.Handover(ctx, path, serveArgs(cfgPath, logFile), update.LaunchEnv(installed))
		}
		if code, ran := launch(ctx, next, start, log, closeLog); ran {
			return code
		}
	}
	canUpdate := vers.Writable()

	s.Driver.Logger = log
	// the hub's signed time: what the tunnel checks signatures against and
	// the clubs take "now" from
	verifier, err := tunnel.NewVerifier(s.Hub.Key, s.Hub.Installation, s.Hub.Secret, nil)
	if err != nil {
		fmt.Fprintf(stderr, "court-gateway: %v\n", err)
		return 2
	}
	s.Driver.Clock = verifier.Now
	clubs, syncer, err := buildInstallation(s, log)
	if err != nil {
		fmt.Fprintf(stderr, "court-gateway: %v\n", err)
		return 2
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	update.StopOnEOF(cancel)
	restart := make(chan struct{}, 1)
	askRestart := func() {
		select {
		case restart <- struct{}{}:
		default:
		}
	}
	// the outbound tunnel to the hub: the only way in
	hint := make(chan struct{}, 1)
	roots, err := tunnel.LoadRoots(s.Hub.CAFile)
	if err != nil {
		fmt.Fprintf(stderr, "court-gateway: %v\n", err)
		return 2
	}
	client, err := tunnel.NewClient(tunnel.ClientConfig{
		HubURL: s.Hub.URL, Secret: s.Hub.Secret, HubKey: s.Hub.Key, Installation: s.Hub.Installation, Verifier: verifier,
		Polls: s.Hub.Polls, Handler: clubs.TunnelHandler(),
		Version: version, Build: build(), Platform: update.Platform(), Logger: log,
		Transport: tunnel.NewTransport(roots, s.Hub.Proxy), OnConfigVersion: syncer.Notify,
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
	syncer.SetFetch(client.FetchConfig)
	go syncer.Run(ctx)
	tunnelDone := make(chan struct{})
	go func() {
		defer close(tunnelDone)
		client.Run(ctx)
	}()

	// every statusInterval a readiness round (syncer.Ready) feeds the
	// health status file (read by `healthcheck`) and a heartbeat to the
	// hub; the file is also rewritten when the hub connection changes
	go func() {
		t := time.NewTicker(min(2*time.Second, statusInterval))
		defer t.Stop()
		// each club has its own time limit (core.ReadyTimeout); this only
		// bounds the whole round
		check := &tunnel.SelfCheck{Ready: syncer.Ready, Reason: core.ReadyReason, Clubs: core.ReadyClubs, Interval: statusInterval,
			Version: version, Build: build()}
		var st core.HealthStatus
		var crm tunnel.Heartbeat // the last self-check
		checked := false
		sentConfig := int64(-1) // the applied config version the hub last heard of
		lastSeen := time.Now()  // the hub, or the start
		files := newConfigWatch(cfgPath, procEnv, s.Driver.Account, log, askRestart)
		clockSkewed := false
		var refused int64    // refused signatures, at the last self-check
		refusedGrew := false // since the self-check before
		unwritable := false  // the status file could not be written last time
		for {
			files.check()
			_, clockKnown := verifier.Now()
			hs := hubState{connected: client.Connected(), authFailed: client.Unauthorized(), duplicate: client.Duplicate(), clockOffset: client.ClockOffset(), clockKnown: clockKnown,
				keyMismatch: client.HubKeyMismatch(), refusedGrew: refusedGrew, stateUnwritable: unwritable}
			if skewed := hs.clockOffset.Abs() > tunnel.MaxClockSkew; skewed != clockSkewed {
				clockSkewed = skewed
				if skewed {
					log.Error("this computer's clock is far off the hub's: the gateway refuses what the hub sends until the clock is set right (turn on automatic time)",
						"offset", hs.clockOffset.Round(time.Second).String())
				}
			}
			if hs.connected {
				lastSeen = time.Now()
			}
			hs.silentFor = time.Since(lastSeen)
			hb, due := check.Step(ctx)
			if ctx.Err() != nil {
				return
			}
			cs := syncer.State()
			hs.configRefused = cs.ConfigError != ""
			hs.configBehind = syncer.Behind()
			if due {
				crm, checked = hb, true
				n := client.Refused()
				refusedGrew, refused = n > refused, n
				hs.refusedGrew = refusedGrew
			}
			ready, reason := readiness(crm.Ready, crm.Reason, hs)
			// a newly applied config is reported at once, not with the
			// next self-check a minute later
			beat := checked && (due || cs.ConfigVersion != sentConfig)
			if beat {
				hb := crm
				hb.Ready, hb.Reason, hb.AppliedConfig = ready, reason, cs
				sentConfig = cs.ConfigVersion
				go sendHeartbeat(ctx, client, hb, log)
			}
			next := core.HealthStatus{Version: version, Ready: ready, Reason: reason, Hub: hs.String()}
			if beat || unwritable || next.Ready != st.Ready || next.Reason != st.Reason || next.Hub != st.Hub {
				next.Time = time.Now().UTC()
				werr := core.WriteHealthStatus(s.StateDir, next)
				if werr != nil && !unwritable {
					log.Error("the state directory is not writable: the gateway reports STATE_UNWRITABLE", "path", s.StateDir, "error", werr)
				}
				if unwritable = werr != nil; !unwritable {
					st = next
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	// a freshly updated binary confirms itself or rolls back: it must log
	// in to the CRM and connect to the hub, as the version before it did;
	// the clubs' own readiness is reported by the heartbeat, not judged here
	if canUpdate {
		go func() {
			again, err := update.Confirm(ctx, vers, version, func(ctx context.Context) error {
				return waitHealthy(ctx, s.Driver.Account.CheckLogin, client.Connected, confirmDeadline)
			}, log, nil)
			if err != nil && ctx.Err() == nil {
				log.Error("update confirmation", "error", err)
			}
			if again {
				askRestart()
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
		log.Warn("the state directory is not writable; automatic updates are off", "path", s.StateDir)
	default:
		u := &update.Updater{
			PublicKey: key, ManifestURL: s.Update.ManifestURL, Current: version, Client: &http.Client{Timeout: 5 * time.Minute, Transport: s.Hub.Proxy.Transport()},
			Versions: vers, CheckBinary: update.ChildCheck(cfgPath), Log: log, Hold: passwordRejected(s.Driver.Account),
		}
		first := 5*time.Minute + time.Duration(time.Now().UnixNano()%int64(10*time.Minute))
		go u.Loop(ctx, hint, first, 24*time.Hour, askRestart)
	}

	log.Info("started", "version", version, "build", build(), "crm", s.CRMType)
	restarting := false
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case <-restart:
		restarting = true
		log.Info("restarting to run the version that is current now")
	}
	cancel()
	// the copy started after this one (a restart, an update, the service
	// manager) must not be refused by the hub as a duplicate
	<-tunnelDone
	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := client.Release(rctx); err != nil {
		log.Debug("hub not told that this copy stopped", "error", err)
	}
	rcancel()
	if restarting {
		// returns only where a process cannot replace itself (Windows) or
		// on failure; then the restart code makes the installed binary or
		// the service manager start again
		if err := restartProcess(installed, serveArgs(cfgPath, logFile)); err != nil && !errors.Is(err, update.ErrRestartRequired) {
			log.Error("restart failed; the service manager will start the binary again", "error", err)
		}
		return exitRestart
	}
	return 0
}

// launch runs the newest verified version (next) with start until it
// exits other than to be restarted; ran is false when this binary runs
// itself. The log file is closed before every start: while the version
// runs, this process holds no handle on it, which on Windows would stop
// the version from rotating it. A later log line opens it again.
func launch(ctx context.Context, next func() string, start func(context.Context, string) (int, error), log *slog.Logger, closeLog func()) (code int, ran bool) {
	for range maxHandovers {
		path := next()
		if path == "" {
			break
		}
		log.Info("starting the newest verified version", "path", path)
		closeLog()
		code, err := start(ctx, path)
		if err != nil {
			log.Error("could not start it, running the installed binary", "error", err)
			break
		}
		if code != exitRestart || ctx.Err() != nil {
			return code, true
		}
	}
	return 0, false
}

// serveArgs are the arguments of a version the installed binary hands
// over to, or of the installed binary started again: serve with this
// config and this log file. Explicit, not this process's own arguments:
// on Windows those are the service's, and the log file is not among them.
func serveArgs(cfgPath, logFile string) []string {
	args := []string{"serve", "-config", cfgPath}
	if logFile != "" {
		args = append(args, "-log-file", logFile)
	}
	return args
}

// buildInstallation builds what serve runs: the CRM account, the
// multi-club handler and the syncer that keeps the clubs equal to the
// installation's config on the hub.
func buildInstallation(s *core.Settings, log *slog.Logger) (*core.Clubs, *remote.Syncer, error) {
	acct, err := core.BuildAccount(s, drivers.Accounts, s.Driver)
	if err != nil {
		return nil, nil, err
	}
	s.Driver.Account = acct // one CRM session for the account and its clubs
	clubs := core.NewClubs(core.ClubsOptions{Logger: log, ReadyNoClubs: acct.CheckLogin})
	clubs.SetGatewayAPI(remote.DiscoverHandler(acct, clubs, log))
	return clubs, remote.New(remote.Options{Base: s, Factories: drivers.Factories, Clubs: clubs, Logger: log}), nil
}

// passwordRejected reports whether the CRM rejected acct's password: then
// no update is checked, since the new binary's self-check would log in
// with that password again.
func passwordRejected(acct driver.Account) func() bool {
	ps, ok := acct.(driver.PasswordSetter)
	return func() bool { return ok && ps.PasswordRejected() }
}

// configWatch notices when setup or set-password rewrites gateway.yaml or
// .env next to it. A new CRM password is taken at once, read from .env
// itself (the redactor learns it first): that ends a stop after a
// rejected login. A changed hub connection (setup --force: a new secret,
// key, installation or address) restarts the service, which then runs
// with it.
type configWatch struct {
	cfgPath string
	procEnv func(string) string
	acct    driver.PasswordSetter
	log     *slog.Logger
	restart func()
	seen    [2]os.FileInfo
	// as the files said when last read
	password, hub string
}

func newConfigWatch(cfgPath string, procEnv func(string) string, acct driver.Account, log *slog.Logger, restart func()) *configWatch {
	w := &configWatch{cfgPath: cfgPath, procEnv: procEnv, log: log, restart: restart}
	w.acct, _ = acct.(driver.PasswordSetter)
	w.seen = w.stat()
	w.password, w.hub, _ = w.read()
	return w
}

func (w *configWatch) stat() (out [2]os.FileInfo) {
	out[0], _ = os.Stat(w.cfgPath)
	out[1], _ = os.Stat(filepath.Join(filepath.Dir(w.cfgPath), core.EnvFileName))
	return out
}

// read returns the CRM password in .env and the hub connection the files
// configure ("" when they do not load), .env taking precedence over the
// process environment.
func (w *configWatch) read() (password, hub string, err error) {
	vals := map[string]string{}
	if data, err := os.ReadFile(filepath.Join(filepath.Dir(w.cfgPath), core.EnvFileName)); err == nil {
		if vals, err = core.ParseDotEnv(data); err != nil {
			return "", "", err
		}
	}
	password = vals[core.PasswordEnv]
	s, err := core.LoadFile(w.cfgPath, func(k string) string { return cmp.Or(vals[k], w.procEnv(k)) })
	if err != nil {
		return password, "", err
	}
	h := s.Hub
	return password, strings.Join([]string{h.URL, h.Installation, core.EncodeHubKey(h.Key), tunnel.SecretSHA256(h.Secret), h.CAFile, strconv.Itoa(h.Polls)}, "\n"), nil
}

func (w *configWatch) check() {
	st := w.stat()
	if same(st[0], w.seen[0]) && same(st[1], w.seen[1]) {
		return
	}
	w.seen = st
	password, hub, err := w.read()
	switch {
	case err != nil:
		w.log.Warn("changed gateway.yaml or .env not read in full", "error", err)
	case hub != w.hub:
		w.log.Info("the hub connection changed in gateway.yaml or .env (setup --force): restarting")
		w.hub = hub
		w.restart()
	}
	if password != "" && password != w.password && w.acct != nil {
		core.AddSecret(w.log, password)
		w.acct.SetPassword(password)
		w.log.Info("CRM password re-read from .env: logging in to the CRM again")
	}
	w.password = cmp.Or(password, w.password)
}

func same(a, b os.FileInfo) bool {
	return (a == nil) == (b == nil) && (a == nil || a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size())
}

// hubOfflineAfter is how long the hub may be unreachable before the
// gateway reports itself not ready (the hub's default offline_after).
const hubOfflineAfter = 3 * time.Minute

// configPendingAfter is how long the gateway may run another club config
// version than the hub's (it could not pull it) before it is not ready.
const configPendingAfter = 3 * time.Minute

// hubState is what readiness needs to know about the hub connection.
type hubState struct {
	connected     bool
	authFailed    bool // the hub rejected the gateway secret
	duplicate     bool // the hub serves another copy with the same secret
	silentFor     time.Duration
	configRefused bool          // a club of the hub's config does not load
	configBehind  time.Duration // how long the hub's config version has not been applied
	clockOffset   time.Duration // the hub's clock minus this computer's, as the hub signed it
	clockKnown    bool          // the hub's time is known (tunnel.Verifier.Now)
	keyMismatch   bool          // the hub signs with another key than the pinned one
	refusedGrew   bool          // signatures were refused since the self-check before
	// the state directory is not writable (the status file, updates)
	stateUnwritable bool
}

func (h hubState) String() string {
	if h.connected {
		return "connected"
	}
	return "disconnected"
}

// readiness combines the CRM self-check with the hub: a gateway the hub
// cannot reach or does not accept, or whose club config was refused, is
// not ready either, with its own reason code.
func readiness(crmReady bool, crmReason string, h hubState) (bool, string) {
	switch {
	case !crmReady:
		return false, cmp.Or(crmReason, "STARTING")
	case h.authFailed:
		return false, "HUB_AUTH_FAILED"
	case h.duplicate:
		return false, "DUPLICATE_GATEWAY"
	case h.stateUnwritable:
		return false, "STATE_UNWRITABLE"
	case h.clockOffset.Abs() > tunnel.MaxClockSkew:
		return false, "CLOCK_SKEW"
	case h.connected && h.keyMismatch:
		return false, "HUB_KEY_MISMATCH" // a new hub key: a new join code is needed
	case h.connected && !h.clockKnown:
		return false, "CLOCK_UNKNOWN" // the hub sends no signed time: nothing is booked
	case h.refusedGrew:
		return false, "SIGNATURE_REFUSED"

	case !h.connected && h.silentFor > hubOfflineAfter:
		return false, "HUB_UNREACHABLE"
	case h.configBehind > configPendingAfter:
		return false, "CONFIG_PENDING"
	case h.configRefused:
		return false, "CONFIG_REFUSED"
	}
	return true, ""
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

// waitHealthy waits until the CRM login works and the hub connection is
// up. A login the CRM still rejects at the deadline is update.ErrNotRun:
// the password, not the binary.
func waitHealthy(ctx context.Context, login func(context.Context) error, connected func() bool, deadline time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	reason := "STARTING"
	for {
		lctx, lcancel := context.WithTimeout(ctx, core.ReadyTimeout)
		err := login(lctx)
		lcancel()
		switch {
		case err != nil:
			reason = "CRM: " + core.ReadyReason(err)
		case !connected():
			reason = "HUB_DISCONNECTED"
		default:
			return nil
		}
		select {
		case <-ctx.Done():
			if reason == "CRM: CRM_AUTH_FAILED" {
				return update.ErrNotRun
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
	hubCfg := fs.String("hub", "", "probe the hub running with this hub.yaml instead of a gateway")
	if fs.Parse(args) != nil {
		return 2
	}
	if *hubCfg != "" {
		return probeHub(*hubCfg, stdout, stderr)
	}
	return probeStatusFile(core.StateDir(*cfgPath, getenv), stdout, stderr)
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
func probeHub(cfgPath string, stdout, stderr io.Writer) int {
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

// cmdSelfcheck is what a running gateway asks a freshly downloaded binary
// to do before switching to it: load the installation's gateway.yaml and
// .env, log in to the CRM, reach the hub with the gateway secret (the
// clubs on the hub are not checked). Exit 0 ok; 1 when the binary cannot
// work with this config; update.SelfCheckTransientExit when the
// environment failed, update.SelfCheckNotRunExit when the CRM rejected
// the password.
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
	var alert *driver.ConfigAlert
	if err := clubs.Ready(ctx); err != nil && !errors.As(err, &alert) { // an alert does not stop the update
		fmt.Fprintln(stderr, "CRM: "+core.ReadyReason(err))
		if core.ReadyReason(err) == "CRM_AUTH_FAILED" {
			return update.SelfCheckNotRunExit // the password, not this binary: one login, no verdict
		}
		return update.SelfCheckTransientExit
	}
	roots, err := tunnel.LoadRoots(s.Hub.CAFile)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if _, err := tunnel.FetchWhoami(ctx, tunnel.NewTransport(roots, s.Hub.Proxy), s.Hub.URL, s.Hub.Secret); err != nil {
		fmt.Fprintln(stderr, "hub: unreachable or secret rejected")
		return update.SelfCheckTransientExit
	}
	fmt.Fprintln(stdout, "ok")
	return 0
}
