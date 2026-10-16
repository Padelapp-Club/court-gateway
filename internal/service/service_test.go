package service

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// On macOS the service must log through the rotating writer: launchd's own
// output file is never rotated, so it may only catch startup errors.
func TestLaunchdLogsThroughTheRotatingFile(t *testing.T) {
	s := Spec{Exe: "/opt/example/court-gateway", Config: "/opt/example/gateway.yaml", System: true, User: "example"}
	p := LaunchdPlist(s)
	if !strings.Contains(p, "<string>-log-file</string>\n    <string>"+s.LogFile()+"</string>") {
		t.Fatalf("the plist does not pass -log-file %s:\n%s", s.LogFile(), p)
	}
	if strings.Contains(p, "<key>StandardErrorPath</key><string>"+s.LogFile()+"<") ||
		!strings.Contains(p, "<key>StandardErrorPath</key><string>"+s.StartupLogFile()+"<") {
		t.Fatalf("launchd output must go to the separate startup file, not the rotated log:\n%s", p)
	}
}

// systemd keeps the log in the journal (bounded by journald); Windows
// services log through the rotating file, set in the service entry point.
func TestSystemdLogsToTheJournal(t *testing.T) {
	u := SystemdUnit(Spec{Exe: "/opt/example/court-gateway", Config: "/opt/example/gateway.yaml"})
	if strings.Contains(u, "StandardOutput=file") || strings.Contains(u, "StandardError=file") || strings.Contains(u, "-log-file") {
		t.Fatalf("the unit must leave the log to journald:\n%s", u)
	}
}

// A path with a space and a % in the unit: the command line quotes and
// escapes it, WorkingDirectory takes it unquoted (quotes would become part
// of the path) with % escaped.
func TestSystemdUnitPaths(t *testing.T) {
	u := SystemdUnit(Spec{Exe: "/opt/court gateway 100%/court-gateway", Config: "/opt/court gateway 100%/gateway.yaml"})
	for _, want := range []string{
		"ExecStart=\"/opt/court gateway 100%%/court-gateway\" serve -config \"/opt/court gateway 100%%/gateway.yaml\"\n",
		"WorkingDirectory=/opt/court gateway 100%%\n",
	} {
		if !strings.Contains(u, want) {
			t.Fatalf("missing %q in\n%s", want, u)
		}
	}
}

// macOS has two paths: the system service for clubs (starts at boot,
// replaces a trial per-user job, which would be a second copy) and the
// per-user LaunchAgent for trials, whose message says that it runs only
// while the user is logged in. Uninstall removes whichever exists.
func TestMacOSPaths(t *testing.T) {
	s := Spec{Exe: "/Users/u/court-gateway/court-gateway", Config: "/Users/u/court-gateway/gateway.yaml", Home: "/Users/u", UID: 501}
	agentPlist, daemonPlist := "/Users/u/Library/LaunchAgents/court-gateway.plist", "/Library/LaunchDaemons/court-gateway.plist"
	has := func(cmds [][]string, want ...string) bool {
		for _, c := range cmds {
			if strings.Join(c, " ") == strings.Join(want, " ") {
				return true
			}
		}
		return false
	}
	user, _ := MakePlan("darwin", s)
	if user.Files[agentPlist] == "" || !strings.Contains(user.Done, "только пока вы вошли") || strings.Contains(user.Done, "стартовать сам после перезагрузки") ||
		!strings.Contains(strings.Join(user.Notes, "\n"), "launchctl print gui/$(id -u)/court-gateway") {
		t.Fatalf("per-user plan: %+v", user)
	}
	s.System = true
	sys, _ := MakePlan("darwin", s)
	if sys.Files[daemonPlist] == "" || !has(sys.Commands, "rm", "-f", agentPlist) || !has(sys.Commands, "launchctl", "bootout", "gui/501/court-gateway") ||
		!strings.Contains(sys.Done, "после перезагрузки") || !strings.Contains(strings.Join(sys.Notes, "\n"), "sudo launchctl print system/court-gateway") {
		t.Fatalf("system plan: %+v", sys)
	}
	for _, system := range []bool{false, true} {
		s.System = system
		un, _ := UninstallPlan("darwin", s)
		if !has(un.Commands, "rm", "-f", agentPlist) || !has(un.Commands, "rm", "-f", daemonPlist) || !has(un.Commands, "launchctl", "bootout", "system/court-gateway") {
			t.Fatalf("uninstall (system=%v) leaves a job: %q", system, un.Commands)
		}
	}
}

// A first install boots out a job that is not loaded yet; launchctl's
// complaint about it is not shown.
func TestBootoutComplaintIsQuiet(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	r, w, _ := os.Pipe()
	saved := os.Stderr
	os.Stderr = w
	err := ExecRunner("sh", "bootout") // sh complains: no such file
	os.Stderr = saved
	w.Close()
	out, _ := io.ReadAll(r)
	if err == nil || len(out) != 0 {
		t.Fatalf("err=%v, shown: %q", err, out)
	}
}

// Query trusts only the manager's own answer about this service: no
// manager, an error or a timeout is Unknown, never Stopped.
func TestQueryAsksTheServiceManager(t *testing.T) {
	type answer struct {
		out  string
		exit int
		err  error
	}
	probe := func(m map[string]answer) Probe {
		return func(name string, args ...string) (string, int, error) {
			a, ok := m[filepath.Base(name)+" "+strings.Join(args, " ")]
			if !ok {
				return "", 0, errors.New("not found")
			}
			return a.out, a.exit, a.err
		}
	}
	timeout := errors.New("context deadline exceeded")
	for name, tc := range map[string]struct {
		goos string
		m    map[string]answer
		want State
	}{
		"systemd active":     {"linux", map[string]answer{"systemctl is-active court-gateway": {"active\n", 0, nil}}, Running},
		"systemd inactive":   {"linux", map[string]answer{"systemctl is-active court-gateway": {"inactive\n", 3, nil}}, Stopped},
		"systemd no bus":     {"linux", map[string]answer{"systemctl is-active court-gateway": {"", 1, nil}}, Unknown},
		"no systemctl":       {"linux", nil, Unknown},
		"systemctl hangs":    {"linux", map[string]answer{"systemctl is-active court-gateway": {"", 0, timeout}}, Unknown},
		"launchd daemon":     {"darwin", map[string]answer{"launchctl print system/court-gateway": {"\tstate = running\n", 0, nil}}, Running},
		"launchd agent":      {"darwin", map[string]answer{"launchctl print system/court-gateway": {"", 113, nil}, "launchctl print gui/501/court-gateway": {"\tstate = running\n", 0, nil}}, Running},
		"launchd none":       {"darwin", map[string]answer{"launchctl print system/court-gateway": {"", 113, nil}, "launchctl print gui/501/court-gateway": {"", 113, nil}}, Stopped},
		"launchd denied":     {"darwin", map[string]answer{"launchctl print system/court-gateway": {"", 1, nil}}, Unknown},
		"windows running":    {"windows", map[string]answer{"sc.exe query court-gateway": {"        STATE              : 4  RUNNING\n", 0, nil}}, Running},
		"windows stopped":    {"windows", map[string]answer{"sc.exe query court-gateway": {"        STATE              : 1  STOPPED\n", 0, nil}}, Stopped},
		"windows no service": {"windows", map[string]answer{"sc.exe query court-gateway": {"[SC] OpenService FAILED 1060:\n", 1060, nil}}, Stopped},
		"windows denied":     {"windows", map[string]answer{"sc.exe query court-gateway": {"", 5, nil}}, Unknown},
		"another OS":         {"freebsd", nil, Unknown},
	} {
		if got := Query(tc.goos, 501, probe(tc.m)); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

// A manager command that hangs is cut off: Unknown within the bound.
func TestRunProbeIsBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no sleep command")
	}
	start := time.Now()
	if _, _, err := RunProbe("/bin/sleep", "60"); err == nil || time.Since(start) > probeTimeout+5*time.Second {
		t.Fatalf("a hanging command: %v after %s", err, time.Since(start))
	}
}
