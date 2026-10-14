package service

import (
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
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
