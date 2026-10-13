package update

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
)

// release is a fake release server: manifest, signature and one binary.
type release struct {
	srv      *httptest.Server
	pub      ed25519.PublicKey
	priv     ed25519.PrivateKey
	manifest []byte
	sig      []byte
	binary   []byte
	hits     atomic.Int32
}

func newRelease(t *testing.T, ver string, binary []byte) *release {
	t.Helper()
	privFile, pubB64, err := GenerateKey() // a throwaway key: never a real one
	if err != nil {
		t.Fatal(err)
	}
	priv, err := ParsePrivateKey(privFile)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParsePublicKey(pubB64)
	if err != nil {
		t.Fatal(err)
	}
	r := &release{pub: pub, priv: priv, binary: binary}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.hits.Add(1)
		switch req.URL.Path {
		case "/manifest.json":
			_, _ = w.Write(r.manifest)
		case "/manifest.json.sig":
			_, _ = w.Write(r.sig)
		case "/bin":
			_, _ = w.Write(r.binary)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(r.srv.Close)
	r.setManifest(t, ver, binary)
	return r
}

func (r *release) setManifest(t *testing.T, ver string, binary []byte) {
	sum := sha256.Sum256(binary)
	m := Manifest{Version: ver, Assets: map[string]Asset{
		"test-plat": {URL: r.srv.URL + "/bin", SHA256: hex.EncodeToString(sum[:])},
	}}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	r.manifest = data
	r.sig = Sign(r.priv, data)
}

type rig struct {
	dir       string
	installed string
	state     string
	v         *Versions
	u         *Updater
}

// newRig is an installation of version current: the installed binary and
// an empty state directory.
func newRig(t *testing.T, r *release, current string) *rig {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "court-gateway")
	if err := os.WriteFile(exe, []byte("INSTALLED BINARY "+current), 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	v := &Versions{Dir: state}
	if !v.Writable() {
		t.Fatal("state not writable")
	}
	u := &Updater{
		PublicKey: r.pub, ManifestURL: r.srv.URL + "/manifest.json", Current: current, Platform: "test-plat",
		Client: r.srv.Client(), Versions: v,
	}
	return &rig{dir: dir, installed: exe, state: state, v: v, u: u}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// untouched fails unless the installed binary is still there, unchanged.
func (g *rig) untouched(t *testing.T) {
	t.Helper()
	if got := read(t, g.installed); !strings.HasPrefix(got, "INSTALLED BINARY ") {
		t.Fatalf("the installed binary changed: %q", got)
	}
}

// runs returns what a start of the installed binary (version own) runs:
// the content of the version it hands over to, or of itself.
func (g *rig) runs(t *testing.T, own string) string {
	t.Helper()
	if next := g.v.Launch(own, time.Now()); next != "" {
		return read(t, next)
	}
	return read(t, g.installed)
}

func noStrays(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".court-gateway-download-") {
			t.Fatalf("download left behind: %s", e.Name())
		}
	}
}

func TestValidSignatureInstalls(t *testing.T) {
	r := newRelease(t, "1.1.0", []byte("NEW BINARY 1.1.0"))
	g := newRig(t, r, "1.0.0")
	installed, err := g.u.Check(context.Background())
	if err != nil || installed != "1.1.0" {
		t.Fatalf("result: %q %v", installed, err)
	}
	g.untouched(t)
	if got := g.runs(t, "1.0.0"); got != "NEW BINARY 1.1.0" {
		t.Fatalf("the installed binary does not hand over: %q", got)
	}
	p, err := ReadPending(g.state)
	if err != nil || p == nil || p.Version != "1.1.0" || p.From != "1.0.0" {
		t.Fatalf("pending: %+v %v", p, err)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(g.v.Launch("1.0.0", time.Now())); info.Mode().Perm()&0o100 == 0 {
			t.Fatalf("new binary not executable: %v", info.Mode())
		}
	}
	noStrays(t, g.v.StagingDir())
}

func TestBadSignatureRejected(t *testing.T) {
	r := newRelease(t, "1.1.0", []byte("NEW BINARY 1.1.0"))
	g := newRig(t, r, "1.0.0")
	// a manifest changed after signing (e.g. pointing at another binary)
	r.manifest = []byte(strings.Replace(string(r.manifest), "1.1.0", "1.2.0", 1))
	if _, err := g.u.Check(context.Background()); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered manifest: %v", err)
	}
	// a manifest signed by another key
	r.setManifest(t, "1.1.0", r.binary)
	_, otherPub, _ := GenerateKey()
	g.u.PublicKey, _ = ParsePublicKey(otherPub)
	if _, err := g.u.Check(context.Background()); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("foreign key: %v", err)
	}
	g.untouched(t)
	if g.v.Launch("1.0.0", time.Now()) != "" {
		t.Fatal("a version was installed despite a bad signature")
	}
	if p, _ := ReadPending(g.state); p != nil {
		t.Fatal("pending marker written")
	}
}

func TestBadChecksumRejected(t *testing.T) {
	r := newRelease(t, "1.1.0", []byte("NEW BINARY 1.1.0"))
	g := newRig(t, r, "1.0.0")
	r.binary = []byte("SOMETHING ELSE ENTIRELY") // the server swapped the file
	if _, err := g.u.Check(context.Background()); !errors.Is(err, ErrChecksum) {
		t.Fatalf("swapped binary: %v", err)
	}
	g.untouched(t)
	if g.v.Launch("1.0.0", time.Now()) != "" {
		t.Fatal("a version was installed despite a checksum mismatch")
	}
	noStrays(t, g.v.StagingDir())
}

func TestNotNewerIsNoop(t *testing.T) {
	for _, ver := range []string{"1.0.0", "0.9.9"} {
		r := newRelease(t, ver, []byte("BIN"))
		g := newRig(t, r, "1.0.0")
		installed, err := g.u.Check(context.Background())
		if err != nil || installed != "" || g.v.Launch("1.0.0", time.Now()) != "" {
			t.Fatalf("%s: %q %v", ver, installed, err)
		}
	}
}

func TestSelfBuiltNeverUpdates(t *testing.T) {
	r := newRelease(t, "9.9.9", []byte("NEW"))
	g := newRig(t, r, "1.0.0")
	g.u.PublicKey = nil // no embedded release key
	if _, err := g.u.Check(context.Background()); !errors.Is(err, ErrSelfBuilt) {
		t.Fatalf("self-built: %v", err)
	}
	if n := r.hits.Load(); n != 0 {
		t.Fatalf("a self-built binary contacted the release server %d times", n)
	}
	g.untouched(t)
}

func TestChildCheckRejectsBrokenBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses shell scripts as stand-in binaries")
	}
	cfg := filepath.Join(t.TempDir(), "gateway.yaml")
	script := func(versionOut string, selfcheckExit int) []byte {
		return []byte("#!/bin/sh\nif [ \"$1\" = version ]; then echo '" + versionOut + "'; exit 0; fi\n" +
			"if [ \"$1\" = selfcheck ]; then echo 'check output' >&2; exit " + strconv.Itoa(selfcheckExit) + "; fi\nexit 99\n")
	}
	cases := []struct {
		name    string
		bin     []byte
		wantErr error
		bad     bool
	}{
		{"passes", script("court-gateway 1.1.0", 0), nil, false},
		{"fails self-check", script("court-gateway 1.1.0", 1), ErrBroken, true},
		{"mislabelled", script("court-gateway 1.0.5", 0), ErrBroken, true},
		{"version prefix only", script("court-gateway 1.1.01 (release, x)", 0), ErrBroken, true},
		{"environment down", script("court-gateway 1.1.0", SelfCheckTransientExit), ErrTransient, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRelease(t, "1.1.0", c.bin)
			g := newRig(t, r, "1.0.0")
			g.u.CheckBinary = ChildCheck(cfg)
			installed, err := g.u.Check(context.Background())
			if c.wantErr == nil {
				if err != nil || installed != "1.1.0" {
					t.Fatalf("%q %v", installed, err)
				}
				return
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
			g.untouched(t)
			if g.v.Launch("1.0.0", time.Now()) != "" {
				t.Fatal("a binary that failed its check was installed")
			}
			if _, bad := readBad(g.state)["1.1.0"]; bad != c.bad {
				t.Fatalf("marked bad = %v, want %v", bad, c.bad)
			}
			noStrays(t, g.v.StagingDir())
		})
	}
}

func TestRollbackWhenNewVersionFailsOnStart(t *testing.T) {
	r := newRelease(t, "1.1.0", []byte("NEW BINARY 1.1.0"))
	g := newRig(t, r, "1.0.0")
	if _, err := g.u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	// the new version starts and cannot become ready
	restart, err := Confirm(context.Background(), g.v, "1.1.0", func(context.Context) error { return errors.New("not ready in time") }, nil, nil)
	if err != nil || !restart {
		t.Fatalf("restart=%v %v", restart, err)
	}
	if got := g.runs(t, "1.0.0"); got != "INSTALLED BINARY 1.0.0" {
		t.Fatalf("not rolled back to the installed binary: %q", got)
	}
	if _, bad := readBad(g.state)["1.1.0"]; !bad {
		t.Fatal("failed version not marked bad")
	}
	if p, _ := ReadPending(g.state); p != nil {
		t.Fatal("pending marker left")
	}
	// and the next daily check does not reinstall it
	if installed, err := g.u.Check(context.Background()); err != nil || installed != "" {
		t.Fatalf("reinstalled a bad version: %q %v", installed, err)
	}
	// once the bad mark is old, it is tried again
	g.u.Now = func() time.Time { return time.Now().Add(BadVersionTTL + time.Hour) }
	if installed, err := g.u.Check(context.Background()); err != nil || installed != "1.1.0" {
		t.Fatalf("after the bad mark expired: %q %v", installed, err)
	}
}

// A failing update goes back to the version before it, not past it.
func TestRollbackToThePreviousVersion(t *testing.T) {
	r := newRelease(t, "1.1.0", []byte("NEW BINARY 1.1.0"))
	g := newRig(t, r, "1.0.0")
	ok := func(context.Context) error { return nil }
	if _, err := g.u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if again, err := Confirm(context.Background(), g.v, "1.1.0", ok, nil, nil); err != nil || again {
		t.Fatalf("confirm 1.1.0: %v %v", again, err)
	}
	r.setManifest(t, "1.2.0", []byte("NEW BINARY 1.2.0"))
	r.binary = []byte("NEW BINARY 1.2.0")
	g.u.Current = "1.1.0"
	if installed, err := g.u.Check(context.Background()); err != nil || installed != "1.2.0" {
		t.Fatalf("install 1.2.0: %q %v", installed, err)
	}
	if got := g.runs(t, "1.0.0"); got != "NEW BINARY 1.2.0" {
		t.Fatalf("runs %q", got)
	}
	if again, err := Confirm(context.Background(), g.v, "1.2.0", func(context.Context) error { return errors.New("down") }, nil, nil); err != nil || !again {
		t.Fatalf("rollback 1.2.0: %v %v", again, err)
	}
	if got := g.runs(t, "1.0.0"); got != "NEW BINARY 1.1.0" {
		t.Fatalf("after rollback runs %q, want 1.1.0", got)
	}
	g.untouched(t)
}

// A pending update that dies before it can confirm or roll back itself is
// rolled back by the installed binary, which counts its starts.
func TestCrashLoopIsRolledBackByTheInstalledBinary(t *testing.T) {
	r := newRelease(t, "1.1.0", []byte("NEW BINARY 1.1.0"))
	g := newRig(t, r, "1.0.0")
	if _, err := g.u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	// the old binary sees a marker that is not about itself: ignore it
	ok := func(context.Context) error { return nil }
	if again, err := Confirm(context.Background(), g.v, "1.0.0", ok, nil, nil); err != nil || again {
		t.Fatalf("old binary: %v %v", again, err)
	}
	if err := writePending(g.state, Pending{Version: "1.1.0", From: "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= MaxStartAttempts; i++ {
		if next := g.v.Next("1.0.0", time.Now(), nil); next == "" || read(t, next) != "NEW BINARY 1.1.0" {
			t.Fatalf("start %d: %q", i, next)
		}
		// ... and the new version dies before Confirm
	}
	if next := g.v.Next("1.0.0", time.Now(), nil); next != "" {
		t.Fatalf("a crash-looping update was started again: %q", next)
	}
	if _, bad := readBad(g.state)["1.1.0"]; !bad {
		t.Fatal("not marked bad")
	}
	if p, _ := ReadPending(g.state); p != nil {
		t.Fatal("pending marker left")
	}
	g.untouched(t)
}

// An update interrupted at any step (power loss, kill) leaves the
// installed binary in place, and whatever a start then picks exists and is
// verified. The installed binary is never renamed or replaced.
func TestInterruptedUpdateLeavesARunnableBinary(t *testing.T) {
	errCrash := errors.New("simulated crash")
	for _, at := range []string{"download", "place", "previous", "current", "prune", "none"} {
		t.Run(at, func(t *testing.T) {
			r := newRelease(t, "1.1.0", []byte("NEW BINARY 1.1.0"))
			g := newRig(t, r, "1.0.0")
			if _, err := g.u.Check(context.Background()); err != nil {
				t.Fatal(err)
			}
			if again, err := Confirm(context.Background(), g.v, "1.1.0", func(context.Context) error { return nil }, nil, nil); err != nil || again {
				t.Fatal(err)
			}
			r.setManifest(t, "1.2.0", []byte("NEW BINARY 1.2.0"))
			r.binary = []byte("NEW BINARY 1.2.0")
			g.u.Current = "1.1.0"
			if at == "download" {
				r.binary = r.binary[:5] // the connection broke mid-download
			}
			step = func(s string) error {
				if s == at {
					return errCrash
				}
				return nil
			}
			defer func() { step = func(string) error { return nil } }()
			_, _ = g.u.Check(context.Background())
			step = func(string) error { return nil }

			g.untouched(t)
			next := g.v.Next("1.0.0", time.Now(), nil)
			got := read(t, g.installed)
			if next != "" {
				got = read(t, next)
			}
			if got != "NEW BINARY 1.1.0" && got != "NEW BINARY 1.2.0" {
				t.Fatalf("a start after the interruption runs %q", got)
			}
			// whichever version runs settles the marker
			ver := strings.TrimPrefix(got, "NEW BINARY ")
			if _, err := Confirm(context.Background(), g.v, ver, func(context.Context) error { return nil }, nil, nil); err != nil {
				t.Fatal(err)
			}
			if p, _ := ReadPending(g.state); p != nil {
				t.Fatalf("marker left: %+v", p)
			}
			if at == "none" && ver != "1.2.0" {
				t.Fatalf("an uninterrupted update runs %s", ver)
			}
		})
	}
}

func TestLaunchSkipsChangedAndBadVersions(t *testing.T) {
	r := newRelease(t, "1.1.0", []byte("NEW BINARY 1.1.0"))
	g := newRig(t, r, "1.0.0")
	if _, err := g.u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _ = Confirm(context.Background(), g.v, "1.1.0", func(context.Context) error { return nil }, nil, nil)
	r.setManifest(t, "1.2.0", []byte("NEW BINARY 1.2.0"))
	r.binary = []byte("NEW BINARY 1.2.0")
	g.u.Current = "1.1.0"
	if _, err := g.u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cur := g.v.Launch("1.0.0", now)
	if g.v.Launch("1.2.0", now) != "" {
		t.Fatal("launched a version that is not newer")
	}
	// a version file changed after verification is never run: the
	// previous one is
	_ = os.WriteFile(cur, []byte("TAMPERED"), 0o755)
	if got := g.runs(t, "1.0.0"); got != "NEW BINARY 1.1.0" {
		t.Fatalf("with the current version changed: %q", got)
	}
	_ = os.WriteFile(cur, []byte("NEW BINARY 1.2.0"), 0o755)
	markBad(g.state, "1.2.0", now)
	if got := g.runs(t, "1.0.0"); got != "NEW BINARY 1.1.0" {
		t.Fatalf("with the current version bad: %q", got)
	}
	markBad(g.state, "1.1.0", now)
	if got := g.runs(t, "1.0.0"); got != "INSTALLED BINARY 1.0.0" {
		t.Fatalf("with both bad: %q", got)
	}
}

func TestLaunchAndRestartEnvironments(t *testing.T) {
	t.Setenv(LaunchedEnv, "1")
	t.Setenv(InstalledEnv, "/opt/example/court-gateway")
	env := strings.Join(LaunchEnv("/opt/example/court-gateway"), "\n")
	if strings.Count(env, LaunchedEnv+"=") != 1 || !strings.Contains(env, InstalledEnv+"=/opt/example/court-gateway") {
		t.Fatalf("launch env: %s", env)
	}
	// a restart starts the installed binary as the service manager does:
	// it must pick the version again
	if env := strings.Join(baseEnv(), "\n"); strings.Contains(env, LaunchedEnv+"=") || strings.Contains(env, InstalledEnv+"=") {
		t.Fatalf("restart env: %s", env)
	}
	if InstalledPath() != "/opt/example/court-gateway" {
		t.Fatalf("installed path: %s", InstalledPath())
	}
}

func TestManifestRequiresHTTPSAssets(t *testing.T) {
	m := `{"version":"1.2.3","assets":{"linux-amd64":{"url":"http://example.com/x","sha256":"` + strings.Repeat("a", 64) + `"}}}`
	if _, err := ParseManifest([]byte(m)); err == nil {
		t.Fatal("plain-http asset accepted")
	}
}

func TestBuildManifest(t *testing.T) {
	dir := t.TempDir()
	for _, p := range Platforms {
		_ = os.WriteFile(filepath.Join(dir, AssetName("1.2.3", p)), []byte(p), 0o644)
	}
	data, err := BuildManifest("1.2.3", dir, "https://example.com/dl", time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest(data)
	if err != nil || len(m.Assets) != len(Platforms) || !strings.HasSuffix(m.Assets["windows-amd64"].URL, ".exe") {
		t.Fatalf("%+v %v", m, err)
	}
}

// The running version is what the service reported, else the verified
// version the installed binary hands over to, else the binary's own.
func TestRunningVersion(t *testing.T) {
	state, now := t.TempDir(), time.Now()
	if got := RunningVersion(state, "1.0.0", now); got != "1.0.0" {
		t.Fatalf("nothing installed: %s", got)
	}
	v := &Versions{Dir: state}
	if err := os.MkdirAll(v.StagingDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	dl := filepath.Join(v.StagingDir(), ".court-gateway-download-1")
	if err := os.WriteFile(dl, []byte("NEW BINARY 1.1.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := v.Install(dl, "1.1.0"); err != nil {
		t.Fatal(err)
	}
	if got := RunningVersion(state, "1.0.0", now); got != "1.1.0" {
		t.Fatalf("with a verified update: %s", got)
	}
	if err := core.WriteHealthStatus(state, core.HealthStatus{Version: "1.2.0"}); err != nil {
		t.Fatal(err)
	}
	if got := RunningVersion(state, "1.0.0", now); got != "1.2.0" {
		t.Fatalf("as the service reports it: %s", got)
	}
}
