// Package update keeps a gateway on the newest signed release without
// anyone at the club doing anything.
//
// A release publishes manifest.json (version, and per platform a download
// URL and SHA-256) and manifest.json.sig (an ed25519 signature over the
// exact manifest bytes, base64). The public key is embedded into release
// builds at link time; a build without it is "self-built" and never
// updates itself. An update is installed only if the signature verifies,
// the version is strictly newer, the download matches the SHA-256 from the
// signed manifest, and the new binary passes a self-check in a child
// process. After the switch the new binary must become ready within a
// deadline, or it puts the previous binary back.
package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/version"
)

// DefaultManifestURL is where release builds look for updates.
const DefaultManifestURL = "https://github.com/Padelapp-Club/court-gateway/releases/latest/download/manifest.json"

// Limits.
const (
	maxManifest = 64 << 10
	maxBinary   = 200 << 20
	// BadVersionTTL: a version that failed after installation is skipped
	// for this long (the failure might have been the network, not the
	// release).
	BadVersionTTL = 72 * time.Hour
)

// Errors.
var (
	ErrSelfBuilt    = errors.New("self-built binary: automatic updates are disabled")
	ErrBadSignature = errors.New("manifest signature does not verify")
	ErrChecksum     = errors.New("downloaded file does not match the manifest checksum")
	// ErrTransient: the new binary could not prove itself because of the
	// environment (CRM or hub unreachable); try again later.
	ErrTransient = errors.New("self-check could not complete")
	// ErrBroken: the new binary does not work here.
	ErrBroken = errors.New("new binary failed its self-check")
)

// Asset is one platform build in a manifest.
type Asset struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size,omitempty"`
}

// Manifest describes one release.
type Manifest struct {
	Version  string           `json:"version"`
	Released string           `json:"released,omitempty"`
	Assets   map[string]Asset `json:"assets"`
}

// Platform is the manifest key of the running build, e.g. "linux-amd64".
func Platform() string { return runtime.GOOS + "-" + runtime.GOARCH }

// ParsePublicKey decodes a base64 ed25519 public key; "" means none.
func ParsePublicKey(b64 string) (ed25519.PublicKey, error) {
	if strings.TrimSpace(b64) == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("release public key is not a base64 ed25519 key")
	}
	return ed25519.PublicKey(raw), nil
}

// Sign returns the base64 signature file content for manifest bytes.
func Sign(priv ed25519.PrivateKey, manifest []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, manifest)) + "\n")
}

// Verify checks a base64 signature file against manifest bytes.
func Verify(pub ed25519.PublicKey, manifest, sigFile []byte) error {
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigFile)))
	if err != nil || len(sig) != ed25519.SignatureSize || len(pub) != ed25519.PublicKeySize {
		return ErrBadSignature
	}
	if !ed25519.Verify(pub, manifest, sig) {
		return ErrBadSignature
	}
	return nil
}

// ParseManifest validates manifest JSON.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if !version.Valid(m.Version) {
		return nil, fmt.Errorf("manifest: bad version %q", m.Version)
	}
	for k, a := range m.Assets {
		if len(a.SHA256) != 64 {
			return nil, fmt.Errorf("manifest: asset %s has no sha256", k)
		}
		if err := core.CheckHTTPSURL("asset", a.URL); err != nil {
			return nil, fmt.Errorf("manifest: asset %s URL must be https", k)
		}
	}
	return &m, nil
}

// Result of a check.
type Result struct {
	Latest    string
	Installed string // version installed by this check, if any
	Target    string // binary to run after installation
}

// Updater checks and installs releases.
type Updater struct {
	PublicKey   ed25519.PublicKey
	ManifestURL string
	// SignatureURL defaults to ManifestURL + ".sig".
	SignatureURL string
	Current      string
	Platform     string
	Client       *http.Client
	StateDir     string
	Installer    Installer
	// CheckBinary runs the downloaded binary's self-check before the
	// switch. Nil skips it (tests only).
	CheckBinary func(ctx context.Context, path, version string) error
	Log         *slog.Logger
	Now         func() time.Time
}

func (u *Updater) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u *Updater) log() *slog.Logger {
	if u.Log != nil {
		return u.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("GET %s: too large", url)
	}
	return data, nil
}

// Fetch downloads and verifies the manifest.
func (u *Updater) Fetch(ctx context.Context) (*Manifest, error) {
	if len(u.PublicKey) == 0 {
		return nil, ErrSelfBuilt
	}
	mURL := u.ManifestURL
	if mURL == "" {
		mURL = DefaultManifestURL
	}
	sURL := u.SignatureURL
	if sURL == "" {
		sURL = mURL + ".sig"
	}
	data, err := u.get(ctx, mURL, maxManifest)
	if err != nil {
		return nil, err
	}
	sig, err := u.get(ctx, sURL, 4096)
	if err != nil {
		return nil, err
	}
	if err := Verify(u.PublicKey, data, sig); err != nil {
		return nil, err
	}
	return ParseManifest(data)
}

// Check fetches the manifest and installs a newer release if there is one.
func (u *Updater) Check(ctx context.Context) (Result, error) {
	m, err := u.Fetch(ctx)
	if err != nil {
		return Result{}, err
	}
	res := Result{Latest: m.Version}
	if !version.Newer(m.Version, u.Current) {
		return res, nil
	}
	if u.isBad(m.Version) {
		u.log().Info("skipping a release that failed here recently", "version", m.Version)
		return res, nil
	}
	plat := u.Platform
	if plat == "" {
		plat = Platform()
	}
	a, ok := m.Assets[plat]
	if !ok {
		return res, fmt.Errorf("release %s has no build for %s", m.Version, plat)
	}
	path, err := u.download(ctx, a)
	if err != nil {
		return res, err
	}
	defer os.Remove(path) // no-op once installed (renamed away)
	if u.CheckBinary != nil {
		if err := u.CheckBinary(ctx, path, m.Version); err != nil {
			if errors.Is(err, ErrBroken) {
				u.markBad(m.Version)
			}
			return res, err
		}
	}
	target, err := u.Installer.Install(path, m.Version)
	if err != nil {
		return res, fmt.Errorf("install: %w", err)
	}
	if err := writePending(u.StateDir, Pending{Version: m.Version, From: u.Current, Installed: u.now().UTC()}); err != nil {
		_ = u.Installer.Rollback()
		return res, fmt.Errorf("install: %w", err)
	}
	res.Installed, res.Target = m.Version, target
	u.log().Info("update installed", "from", u.Current, "to", m.Version)
	return res, nil
}

func (u *Updater) download(ctx context.Context, a Asset) (string, error) {
	want, err := hex.DecodeString(a.SHA256)
	if err != nil || len(want) != sha256.Size {
		return "", ErrChecksum
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := u.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	dir := u.Installer.StagingDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".court-gateway-download-*")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxBinary+1))
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err == nil && n > maxBinary {
		err = errors.New("download: file too large")
	}
	if err == nil && !equalBytes(h.Sum(nil), want) {
		err = ErrChecksum
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o755)
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// ---- bookkeeping in the state directory ----

const (
	pendingFile = "update-pending.json"
	badFile     = "update-bad-versions.json"
)

// Pending marks an installed but not yet confirmed update.
type Pending struct {
	Version   string    `json:"version"`
	From      string    `json:"from"`
	Installed time.Time `json:"installed"`
	Attempts  int       `json:"attempts"`
}

func writePending(dir string, p Pending) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, _ := json.Marshal(p)
	return core.WriteFileAtomic(filepath.Join(dir, pendingFile), data, 0o600)
}

// ReadPending returns the pending update marker, if any.
func ReadPending(dir string) (*Pending, error) {
	data, err := os.ReadFile(filepath.Join(dir, pendingFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Pending
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func clearPending(dir string) error {
	err := os.Remove(filepath.Join(dir, pendingFile))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (u *Updater) badList() map[string]time.Time {
	out := map[string]time.Time{}
	data, err := os.ReadFile(filepath.Join(u.StateDir, badFile))
	if err == nil {
		_ = json.Unmarshal(data, &out)
	}
	return out
}

func (u *Updater) isBad(v string) bool {
	t, ok := u.badList()[v]
	return ok && u.now().Sub(t) < BadVersionTTL
}

func (u *Updater) markBad(v string) { markBad(u.StateDir, v, u.now()) }

func markBad(dir, v string, now time.Time) {
	m := map[string]time.Time{}
	if data, err := os.ReadFile(filepath.Join(dir, badFile)); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	m[v] = now.UTC()
	data, _ := json.Marshal(m)
	_ = os.MkdirAll(dir, 0o700)
	_ = core.WriteFileAtomic(filepath.Join(dir, badFile), data, 0o600)
}

// BadVersions returns the versions that failed here (tests, doctor).
func BadVersions(dir string) map[string]time.Time {
	return (&Updater{StateDir: dir}).badList()
}

// ---- confirmation after the switch ----

// MaxStartAttempts: a pending update that has started this many times
// without confirming itself is rolled back without further checks.
const MaxStartAttempts = 3

// Confirm runs in a freshly started binary. When the state directory says
// this very version was just installed, it waits for check (the gateway
// becoming ready) and either confirms the update or rolls it back. It
// returns the binary to re-execute after a rollback, or "".
func Confirm(ctx context.Context, stateDir, current string, inst Installer, check func(context.Context) error, log *slog.Logger, now func() time.Time) (string, error) {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p, err := ReadPending(stateDir)
	if err != nil || p == nil {
		return "", err
	}
	if p.Version != current {
		// we are not the binary the marker is about: the switch did not
		// happen or was undone; nothing to confirm
		return "", clearPending(stateDir)
	}
	p.Attempts++
	if err := writePending(stateDir, *p); err != nil {
		return "", err
	}
	if p.Attempts <= MaxStartAttempts {
		if err := check(ctx); err == nil {
			log.Info("update confirmed", "version", current, "from", p.From)
			return "", clearPending(stateDir)
		} else if ctx.Err() != nil {
			return "", ctx.Err() // shutting down: decide on the next start
		} else {
			log.Error("updated binary failed its start check, rolling back", "version", current, "error", err)
		}
	} else {
		log.Error("updated binary keeps restarting, rolling back", "version", current, "attempts", p.Attempts)
	}
	markBad(stateDir, current, now())
	if err := inst.Rollback(); err != nil {
		return "", fmt.Errorf("rollback: %w", err)
	}
	if err := clearPending(stateDir); err != nil {
		return "", err
	}
	return inst.Current(), nil
}

// Loop checks after first, then every interval (with up to an hour of
// jitter so a fleet does not hit the release server at once), and when
// trigger fires — the hub's hint — at most once an hour. onInstalled is
// called after a successful installation; the caller restarts into the
// new binary.
func (u *Updater) Loop(ctx context.Context, trigger <-chan struct{}, first, interval time.Duration, onInstalled func(Result)) {
	timer := time.NewTimer(first)
	defer timer.Stop()
	var lastHint time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-trigger:
			if !lastHint.IsZero() && u.now().Sub(lastHint) < time.Hour {
				continue
			}
			lastHint = u.now()
		case <-timer.C:
			timer.Reset(interval + jitter(time.Hour))
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		res, err := u.Check(cctx)
		cancel()
		switch {
		case err != nil:
			u.log().Warn("update check failed", "error", err)
		case res.Installed != "":
			onInstalled(res)
			return
		}
	}
}

func jitter(max time.Duration) time.Duration {
	var b [8]byte
	_, _ = rand.Read(b[:])
	var n uint64
	for _, v := range b {
		n = n<<8 | uint64(v)
	}
	return time.Duration(n % uint64(max))
}
