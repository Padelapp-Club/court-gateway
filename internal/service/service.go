// Package service registers the gateway with the operating system's
// service manager so it starts at boot and is restarted when it exits:
// systemd on Linux, launchd on macOS, the Service Control Manager on
// Windows. Nothing third-party is needed on any of them.
package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Name is the service name on every OS.
const Name = "court-gateway"

// Spec describes the service.
type Spec struct {
	Exe    string // absolute path of the binary
	Config string // absolute path of gateway.yaml
	// User runs the service on Linux and as a macOS daemon; empty means
	// the service manager's default (root / LocalSystem).
	User string
	// System: on macOS a LaunchDaemon (starts at boot, needs sudo) rather
	// than a LaunchAgent (runs while the user is logged in).
	System bool
	// Home and UID are the user's (under sudo: the user who called it):
	// where a macOS LaunchAgent lives.
	Home string
	UID  int
}

// agent is s's macOS LaunchAgent: the per-user job.
func (s Spec) agent() Spec { s.System = false; return s }

func (s Spec) dir() string { return filepath.Dir(s.Config) }

// LogFile is where the service writes its log on macOS and Windows; serve
// rotates it at 10 MB (-log-file).
func (s Spec) LogFile() string { return filepath.Join(s.dir(), "court-gateway.log") }

// StartupLogFile receives what the process writes to stderr before its
// logger exists (a config error) or outside it (a crash) on macOS; serve
// truncates it at start when it has grown past 1 MB.
func (s Spec) StartupLogFile() string { return filepath.Join(s.dir(), "court-gateway.stderr.log") }

// SystemdPath is the unit file location.
const SystemdPath = "/etc/systemd/system/court-gateway.service"

// systemdQuote quotes one word of a command line (ExecStart=): systemd
// splits it at spaces, unescapes \ and ", and expands % and $.
func systemdQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(s) + `"`
}

// systemdPath writes a path setting (WorkingDirectory=), which systemd
// takes as is — spaces included, quotes would be part of the path — and
// only expands % in.
func systemdPath(s string) string { return strings.ReplaceAll(s, "%", "%%") }

// SystemdUnit renders the Linux unit. Restart=always brings the gateway
// back after a crash, after an update re-exec that failed, and after boot.
func SystemdUnit(s Spec) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=court-gateway (partner API of the club; outbound connection to the hub only)\n")
	b.WriteString("Wants=network-online.target\nAfter=network-online.target\n\n")
	b.WriteString("[Service]\nType=simple\n")
	fmt.Fprintf(&b, "ExecStart=%s serve -config %s\n", systemdQuote(s.Exe), systemdQuote(s.Config))
	fmt.Fprintf(&b, "WorkingDirectory=%s\n", systemdPath(s.dir()))
	if s.User != "" {
		fmt.Fprintf(&b, "User=%s\n", s.User)
	}
	b.WriteString("Restart=always\nRestartSec=5\n")
	b.WriteString("NoNewPrivileges=true\nPrivateTmp=true\n\n")
	b.WriteString("[Install]\nWantedBy=multi-user.target\n")
	return b.String()
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// PlistPath is the launchd job file location.
func PlistPath(s Spec) string {
	if s.System {
		return "/Library/LaunchDaemons/" + Name + ".plist"
	}
	return filepath.Join(s.Home, "Library", "LaunchAgents", Name+".plist")
}

// LaunchdPlist renders the macOS job. KeepAlive restarts the gateway
// whenever it exits; RunAtLoad starts it at boot (daemon) or login (agent).
// The log goes to the rotating LogFile; launchd's own stdout/stderr file
// only catches what is written outside the logger, and is bounded by serve.
func LaunchdPlist(s Spec) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + Name + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + xmlEscape(s.Exe) + `</string>
    <string>serve</string>
    <string>-config</string>
    <string>` + xmlEscape(s.Config) + `</string>
    <string>-log-file</string>
    <string>` + xmlEscape(s.LogFile()) + `</string>
  </array>
  <key>WorkingDirectory</key><string>` + xmlEscape(s.dir()) + `</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>StandardOutPath</key><string>` + xmlEscape(s.StartupLogFile()) + `</string>
  <key>StandardErrorPath</key><string>` + xmlEscape(s.StartupLogFile()) + `</string>
`)
	if s.System && s.User != "" {
		b.WriteString("  <key>UserName</key><string>" + xmlEscape(s.User) + "</string>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// WindowsCommands register the service with sc.exe: automatic (delayed)
// start, and recovery actions that restart it after any exit — which is
// also how an update takes effect on Windows. The install folder and .env
// are closed (ProtectSecrets) before these run.
func WindowsCommands(s Spec) [][]string {
	bin := `"` + s.Exe + `" service -config "` + s.Config + `"`
	return [][]string{
		{"sc.exe", "create", Name, "binPath=", bin, "start=", "delayed-auto", "DisplayName=", Name},
		{"sc.exe", "description", Name, "Partner API of the club; connects out to the hub, opens no ports."},
		{"sc.exe", "failure", Name, "reset=", "86400", "actions=", "restart/5000/restart/5000/restart/30000"},
		{"sc.exe", "failureflag", Name, "1"},
		{"sc.exe", "start", Name},
	}
}

// Runner executes a command (replaced in tests).
type Runner func(name string, args ...string) error

// ExecRunner runs commands with output to the terminal, except a
// "bootout", whose complaint about a job that is not loaded is expected.
func ExecRunner(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if len(args) == 0 || args[0] != "bootout" {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	}
	return cmd.Run()
}

// Plan is what Install will do on goos: files to write and commands to run.
type Plan struct {
	Files    map[string]string
	Commands [][]string
	// Done and Notes are shown to the person after installation.
	Done  string
	Notes []string
}

// MakePlan builds the installation plan for goos.
func MakePlan(goos string, s Spec) (Plan, error) {
	p := Plan{Files: map[string]string{}}
	switch goos {
	case "linux":
		p.Files[SystemdPath] = SystemdUnit(s)
		p.Commands = [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "enable", "--now", Name}}
		p.Notes = []string{"журнал: journalctl -u " + Name + " -f"}
	case "darwin":
		path := PlistPath(s)
		p.Files[path] = LaunchdPlist(s)
		domain := launchdDomain(s)
		p.Commands = [][]string{{"launchctl", "bootout", domain + "/" + Name}, {"launchctl", "bootstrap", domain, path}}
		p.Notes = []string{"проверка: " + LaunchdCheck(s), "журнал: " + s.LogFile()}
		if s.System {
			// a trial per-user job would run a second copy: it goes
			p.Commands = append([][]string{{"launchctl", "bootout", launchdDomain(s.agent()) + "/" + Name}, {"rm", "-f", PlistPath(s.agent())}}, p.Commands...)
		} else {
			p.Done = "Служба пользователя (LaunchAgent) установлена и запущена. Она работает, только пока вы вошли в систему, " +
				"и после перезагрузки стартует при вашем входе — так годится для пробы. Для клуба нужна системная служба, " +
				"которая работает с момента включения Mac: sudo ./court-gateway install-service (служба пользователя при этом снимается)."
		}
	case "windows":
		p.Commands = WindowsCommands(s)
		p.Notes = []string{"папка " + s.dir() + " и .env закрываются от всех, кроме администраторов и системы", "журнал: " + s.LogFile()}
	default:
		return p, fmt.Errorf("установка службы на %s не поддерживается", goos)
	}
	if p.Done == "" {
		p.Done = "Служба установлена и запущена; шлюз будет стартовать сам после перезагрузки."
	}
	return p, nil
}

// launchdDomain is where s's launchd job lives.
func launchdDomain(s Spec) string {
	if s.System {
		return "system"
	}
	return "gui/" + strconv.Itoa(s.UID)
}

// LaunchdCheck is the command that shows whether s's launchd job runs.
func LaunchdCheck(s Spec) string {
	if s.System {
		return "sudo launchctl print system/" + Name + " | grep state"
	}
	return "launchctl print gui/$(id -u)/" + Name + " | grep state"
}

// Install writes the files and runs the commands of the plan. A failing
// "bootout" (no previous job) is expected and ignored.
func Install(p Plan, run Runner, write func(path, data string) error) error {
	for path, data := range p.Files {
		if err := write(path, data); err != nil {
			return fmt.Errorf("не удалось записать %s: %w (нужны права администратора: sudo)", path, err)
		}
	}
	for _, c := range p.Commands {
		err := run(c[0], c[1:]...)
		if err != nil && !(len(c) > 1 && c[1] == "bootout") {
			return fmt.Errorf("%s: %w", strings.Join(c, " "), err)
		}
	}
	return nil
}

// UninstallPlan stops and removes the service.
func UninstallPlan(goos string, s Spec) (Plan, error) {
	p := Plan{Files: map[string]string{}}
	switch goos {
	case "linux":
		p.Commands = [][]string{{"systemctl", "disable", "--now", Name}, {"rm", "-f", SystemdPath}, {"systemctl", "daemon-reload"}}
	case "darwin":
		// whichever exists: the user's job and the system one (whose file
		// only sudo can remove)
		sys := s
		sys.System = true
		p.Commands = [][]string{{"launchctl", "bootout", launchdDomain(s.agent()) + "/" + Name}, {"rm", "-f", PlistPath(s.agent())},
			{"launchctl", "bootout", "system/" + Name}, {"rm", "-f", PlistPath(sys)}}
	case "windows":
		p.Commands = [][]string{{"sc.exe", "stop", Name}, {"sc.exe", "delete", Name}}
	default:
		return p, errors.New("не поддерживается")
	}
	return p, nil
}

// CurrentSpec fills a Spec for this machine and process.
func CurrentSpec(cfgPath string, system bool) (Spec, error) {
	exe, err := os.Executable()
	if err != nil {
		return Spec{}, err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	cfg, err := filepath.Abs(cfgPath)
	if err != nil {
		return Spec{}, err
	}
	s := Spec{Exe: exe, Config: cfg, System: system}
	s.Home, _ = os.UserHomeDir()
	if runtime.GOOS != "windows" {
		s.UID = os.Getuid()
		// run as the person who called sudo, not as root
		if u := os.Getenv("SUDO_USER"); u != "" && os.Getuid() == 0 {
			s.User = u
			if who, err := user.Lookup(u); err == nil {
				s.Home = who.HomeDir
				s.UID, _ = strconv.Atoi(who.Uid)
			}
		}
	}
	return s, nil
}

// State is what the service manager says of the gateway's service.
type State int

const (
	// Unknown: the manager could not be asked or gave no clear answer.
	Unknown State = iota
	// Running: the service runs (or is starting or stopping).
	Running
	// Stopped: the service is not installed, or installed and stopped.
	Stopped
)

// Probe runs one manager command: its output and exit code; err when it
// did not run to an exit code (not found, timed out).
type Probe func(name string, args ...string) (out string, exit int, err error)

// Query asks goos's service manager about the gateway's service with probe.
// Only an answer the manager gives for this service is Running or Stopped;
// anything else (no manager, an error, a timeout) is Unknown.
func Query(goos string, uid int, probe Probe) State {
	switch goos {
	case "linux":
		out, exit, err := probe(systemctlPath(), "is-active", Name)
		switch state := strings.TrimSpace(out); {
		case err != nil:
			return Unknown
		case exit == 0 || state == "activating" || state == "deactivating" || state == "reloading":
			return Running
		case exit == 3 && (state == "inactive" || state == "failed" || state == "unknown"):
			return Stopped
		}
		return Unknown
	case "darwin":
		// both the system job and the user's agent; 113: no such job
		for _, domain := range []string{"system", "gui/" + strconv.Itoa(uid)} {
			out, exit, err := probe("/bin/launchctl", "print", domain+"/"+Name)
			switch {
			case err != nil || (exit != 0 && exit != 113):
				return Unknown
			case exit == 0 && strings.Contains(out, "state = running"):
				return Running
			}
		}
		return Stopped
	case "windows":
		out, exit, err := probe(filepath.Join(cmp.Or(os.Getenv("SystemRoot"), `C:\Windows`), "System32", "sc.exe"), "query", Name)
		switch {
		case err != nil:
			return Unknown
		case exit == 1060: // the service does not exist
			return Stopped
		case exit != 0:
			return Unknown
		case strings.Contains(out, "STOPPED"):
			return Stopped
		case strings.Contains(out, "RUNNING") || strings.Contains(out, "_PENDING"):
			return Running
		}
		return Unknown
	}
	return Unknown
}

// systemctlPath is systemctl where the system keeps it.
func systemctlPath() string {
	for _, p := range []string{"/usr/bin/systemctl", "/bin/systemctl"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "/usr/bin/systemctl"
}

// probeTimeout bounds one manager command; probeMaxOutput what is read of it.
const (
	probeTimeout   = 5 * time.Second
	probeMaxOutput = 64 << 10
)

// RunProbe is the Probe of this computer: the command by its absolute
// path, bounded in time and output.
func RunProbe(name string, args ...string) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out limitedBuffer
	cmd.Stdout = &out
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return "", 0, ctx.Err()
	case err == nil:
		return out.String(), 0, nil
	case errors.As(err, &ee) && ee.ExitCode() >= 0:
		return out.String(), ee.ExitCode(), nil
	}
	return "", 0, err
}

// limitedBuffer keeps the first probeMaxOutput bytes written to it.
type limitedBuffer struct{ strings.Builder }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := probeMaxOutput - b.Len(); room > 0 {
		b.Builder.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
