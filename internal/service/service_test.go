package service

import (
	"path/filepath"
	"slices"
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

// On Windows install-service first closes the install folder to everyone
// but Administrators and SYSTEM, by SID (group names are localized).
func TestWindowsPlanRestrictsTheFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "court gateway") // a native path: the plan is built on Windows
	p, err := MakePlan("windows", Spec{Exe: filepath.Join(dir, "court-gateway.exe"), Config: filepath.Join(dir, "gateway.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"icacls", dir, "/inheritance:r", "/grant:r", "*S-1-5-32-544:(OI)(CI)F", "*S-1-5-18:(OI)(CI)F"}
	if !slices.Equal(p.Commands[0], want) {
		t.Fatalf("first command %q, want %q", p.Commands[0], want)
	}
}
