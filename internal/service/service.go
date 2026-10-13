// Package service registers the gateway with the operating system's
// service manager so it starts at boot and is restarted when it exits:
// systemd on Linux, launchd on macOS, the Service Control Manager on
// Windows. Nothing third-party is needed on any of them.
package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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
	// Home is the user's home directory (macOS LaunchAgent location).
	Home string
	UID  int
}

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

func systemdQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// SystemdUnit renders the Linux unit. Restart=always brings the gateway
// back after a crash, after an update re-exec that failed, and after boot.
func SystemdUnit(s Spec) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=court-gateway (partner API of the club; outbound connection to the hub only)\n")
	b.WriteString("Wants=network-online.target\nAfter=network-online.target\n\n")
	b.WriteString("[Service]\nType=simple\n")
	fmt.Fprintf(&b, "ExecStart=%s serve -config %s\n", systemdQuote(s.Exe), systemdQuote(s.Config))
	fmt.Fprintf(&b, "WorkingDirectory=%s\n", systemdQuote(s.dir()))
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

// WindowsCommands are the sc.exe invocations that register the service:
// automatic (delayed) start, and recovery actions that restart it after
// any exit — which is also how an update takes effect on Windows.
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

// ExecRunner runs commands with output to the terminal.
func ExecRunner(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// Plan is what Install will do on goos: files to write and commands to run.
type Plan struct {
	Files    map[string]string
	Commands [][]string
	// Notes are shown to the person after installation.
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
		domain := "gui/" + strconv.Itoa(s.UID)
		if s.System {
			domain = "system"
		}
		p.Commands = [][]string{{"launchctl", "bootout", domain + "/" + Name}, {"launchctl", "bootstrap", domain, path}}
		p.Notes = []string{"журнал: " + s.LogFile()}
		if !s.System {
			p.Notes = append(p.Notes, "служба пользователя работает, пока пользователь вошёл в систему; чтобы шлюз работал с момента включения, запустите install-service через sudo")
		}
	case "windows":
		p.Commands = WindowsCommands(s)
		p.Notes = []string{"журнал: " + s.LogFile()}
	default:
		return p, fmt.Errorf("установка службы на %s не поддерживается", goos)
	}
	return p, nil
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
		domain := "gui/" + strconv.Itoa(s.UID)
		if s.System {
			domain = "system"
		}
		p.Commands = [][]string{{"launchctl", "bootout", domain + "/" + Name}, {"rm", "-f", PlistPath(s)}}
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
		}
	}
	return s, nil
}
