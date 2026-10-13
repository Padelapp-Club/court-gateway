package service

import (
	"path/filepath"
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

// On Windows install-service closes the folder in process
// (ProtectSecrets), not with icacls: the plan runs no ACL command.
func TestWindowsPlanHasNoACLCommand(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "court gateway")
	p, err := MakePlan("windows", Spec{Exe: filepath.Join(dir, "court-gateway.exe"), Config: filepath.Join(dir, "gateway.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range p.Commands {
		if c[0] != "sc.exe" {
			t.Fatalf("unexpected command %q", c)
		}
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
