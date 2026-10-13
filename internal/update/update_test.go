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
	dir   string
	exe   string
	state string
	u     *Updater
}

func newRig(t *testing.T, r *release, current string) *rig {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "court-gateway")
	if err := os.WriteFile(exe, []byte("OLD BINARY "+current), 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	u := &Updater{
		PublicKey: r.pub, ManifestURL: r.srv.URL + "/manifest.json", Current: current, Platform: "test-plat",
		Client: r.srv.Client(), StateDir: state, Installer: &InPlace{Exe: exe},
	}
	return &rig{dir: dir, exe: exe, state: state, u: u}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
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
	res, err := g.u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Installed != "1.1.0" || res.Target != g.exe {
		t.Fatalf("result: %+v", res)
	}
	if read(t, g.exe) != "NEW BINARY 1.1.0" || read(t, g.exe+".prev") != "OLD BINARY 1.0.0" {
		t.Fatal("binary not switched, or previous not kept")
	}
	p, err := ReadPending(g.state)
	if err != nil || p == nil || p.Version != "1.1.0" || p.From != "1.0.0" {
		t.Fatalf("pending: %+v %v", p, err)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(g.exe); info.Mode().Perm()&0o100 == 0 {
			t.Fatalf("new binary not executable: %v", info.Mode())
		}
	}
	noStrays(t, g.dir)
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
	if read(t, g.exe) != "OLD BINARY 1.0.0" {
		t.Fatal("binary changed despite a bad signature")
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
	if read(t, g.exe) != "OLD BINARY 1.0.0" {
		t.Fatal("binary changed despite a checksum mismatch")
	}
	noStrays(t, g.dir)
}

func TestNotNewerIsNoop(t *testing.T) {
	for _, ver := range []string{"1.0.0", "0.9.9"} {
		r := newRelease(t, ver, []byte("BIN"))
		g := newRig(t, r, "1.0.0")
		res, err := g.u.Check(context.Background())
		if err != nil || res.Installed != "" {
			t.Fatalf("%s: %+v %v", ver, res, err)
		}
		if read(t, g.exe) != "OLD BINARY 1.0.0" {
			t.Fatalf("%s: downgraded", ver)
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
	if read(t, g.exe) != "OLD BINARY 1.0.0" {
		t.Fatal("self-built binary changed")
	}
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
		{"environment down", script("court-gateway 1.1.0", SelfCheckTransientExit), ErrTransient, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRelease(t, "1.1.0", c.bin)
			g := newRig(t, r, "1.0.0")
			g.u.CheckBinary = ChildCheck(cfg)
			res, err := g.u.Check(context.Background())
			if c.wantErr == nil {
				if err != nil || res.Installed != "1.1.0" {
					t.Fatalf("%+v %v", res, err)
				}
				return
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
			if read(t, g.exe) != "OLD BINARY 1.0.0" {
				t.Fatal("a binary that failed its check was installed")
			}
			if _, bad := BadVersions(g.state)["1.1.0"]; bad != c.bad {
				t.Fatalf("marked bad = %v, want %v", bad, c.bad)
			}
			noStrays(t, g.dir)
		})
	}
}

func TestRollbackWhenNewBinaryFailsOnStart(t *testing.T) {
	r := newRelease(t, "1.1.0", []byte("NEW BINARY 1.1.0"))
	g := newRig(t, r, "1.0.0")
	if _, err := g.u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	// the new binary starts and cannot become ready
	target, err := Confirm(context.Background(), g.state, "1.1.0", g.u.Installer,
		func(context.Context) error { return errors.New("not ready in time") }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if target != g.exe || read(t, g.exe) != "OLD BINARY 1.0.0" {
		t.Fatalf("not rolled back: target %q content %q", target, read(t, g.exe))
	}
	if _, bad := BadVersions(g.state)["1.1.0"]; !bad {
		t.Fatal("failed version not marked bad")
	}
	if p, _ := ReadPending(g.state); p != nil {
		t.Fatal("pending marker left")
	}
	// and the next daily check does not reinstall it
	res, err := g.u.Check(context.Background())
	if err != nil || res.Installed != "" {
		t.Fatalf("reinstalled a bad version: %+v %v", res, err)
	}
	// once the bad mark is old, it is tried again
	g.u.Now = func() time.Time { return time.Now().Add(BadVersionTTL + time.Hour) }
	if res, err := g.u.Check(context.Background()); err != nil || res.Installed != "1.1.0" {
		t.Fatalf("after the bad mark expired: %+v %v", res, err)
	}
}

func TestConfirmSuccessAndCrashLoop(t *testing.T) {
	r := newRelease(t, "1.1.0", []byte("NEW BINARY 1.1.0"))
	g := newRig(t, r, "1.0.0")
	if _, err := g.u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	ok := func(context.Context) error { return nil }
	// the old binary sees a marker that is not about itself: ignore it
	if target, err := Confirm(context.Background(), g.state, "1.0.0", g.u.Installer, ok, nil, nil); err != nil || target != "" {
		t.Fatalf("old binary: %q %v", target, err)
	}
	if read(t, g.exe) != "NEW BINARY 1.1.0" {
		t.Fatal("old binary undid the update")
	}
	// crash loop: the new binary dies before confirming, three times
	if err := writePending(g.state, Pending{Version: "1.1.0", From: "1.0.0", Attempts: MaxStartAttempts}); err != nil {
		t.Fatal(err)
	}
	called := false
	target, err := Confirm(context.Background(), g.state, "1.1.0", g.u.Installer, func(context.Context) error { called = true; return nil }, nil, nil)
	if err != nil || called || target != g.exe || read(t, g.exe) != "OLD BINARY 1.0.0" {
		t.Fatalf("crash loop: called=%v target=%q %v", called, target, err)
	}
	// a healthy start confirms
	if _, err := (&InPlace{Exe: g.exe}).Install(writeTemp(t, g.dir, "NEW BINARY 1.1.0"), "1.1.0"); err != nil {
		t.Fatal(err)
	}
	if err := writePending(g.state, Pending{Version: "1.1.0", From: "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if target, err := Confirm(context.Background(), g.state, "1.1.0", g.u.Installer, ok, nil, nil); err != nil || target != "" {
		t.Fatalf("confirm: %q %v", target, err)
	}
	if p, _ := ReadPending(g.state); p != nil || read(t, g.exe) != "NEW BINARY 1.1.0" {
		t.Fatal("not confirmed")
	}
}

func writeTemp(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, "staged")
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCacheDirForReadOnlyImage(t *testing.T) {
	r := newRelease(t, "1.1.0", []byte("NEW BINARY 1.1.0"))
	dir := t.TempDir()
	shipped := filepath.Join(dir, "image", "court-gateway")
	_ = os.MkdirAll(filepath.Dir(shipped), 0o755)
	_ = os.WriteFile(shipped, []byte("IMAGE BINARY 1.0.0"), 0o755)
	state := filepath.Join(dir, "state")
	cache := &CacheDir{Dir: state, Shipped: shipped}
	u := &Updater{PublicKey: r.pub, ManifestURL: r.srv.URL + "/manifest.json", Current: "1.0.0", Platform: "test-plat",
		Client: r.srv.Client(), StateDir: state, Installer: cache}
	res, err := u.Check(context.Background())
	if err != nil || res.Installed != "1.1.0" {
		t.Fatalf("%+v %v", res, err)
	}
	if read(t, shipped) != "IMAGE BINARY 1.0.0" {
		t.Fatal("the shipped binary was modified")
	}
	now := time.Now()
	got := cache.Launch("1.0.0", now)
	if got == "" || read(t, got) != "NEW BINARY 1.1.0" {
		t.Fatalf("launch: %q", got)
	}
	if cache.Launch("1.1.0", now) != "" {
		t.Fatal("launched a binary that is not newer")
	}
	// a cached file changed after verification is never run
	_ = os.WriteFile(got, []byte("TAMPERED"), 0o755)
	if cache.Launch("1.0.0", now) != "" {
		t.Fatal("launched a tampered cached binary")
	}
	_ = os.WriteFile(got, []byte("NEW BINARY 1.1.0"), 0o755)
	// rollback returns to the shipped binary
	if err := cache.Rollback(); err != nil {
		t.Fatal(err)
	}
	if cache.Launch("1.0.0", now) != "" || cache.Current() != shipped {
		t.Fatal("rollback did not return to the shipped binary")
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
