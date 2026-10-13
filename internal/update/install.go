package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/version"
)

// Versions keeps verified updates in the state directory and never touches
// the installed binary, the one the service manager starts. That binary
// is a stable launcher: at start it hands over to the newest verified
// version in <state>/versions (Launch), and runs itself when there is
// none. Every step of an update is a new file or an atomic rename inside
// <state>/versions, so an interruption at any point leaves the installed
// binary in place and runnable, and at most an unused file behind.
//
//	versions/court-gateway-<version>[.exe]  verified binaries
//	versions/current.json                   the version to run
//	versions/previous.json                  the one before it (rollback)
type Versions struct {
	// Dir is the state directory.
	Dir string
}

type pointer struct {
	Version string `json:"version"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
}

func (v *Versions) dir() string { return filepath.Join(v.Dir, "versions") }

func (v *Versions) file(name string) string { return filepath.Join(v.dir(), name) }

// StagingDir is where downloads go: the same directory as the versions,
// so the final step is an atomic rename.
func (v *Versions) StagingDir() string { return v.dir() }

// Writable reports whether updates can be kept (the state directory is
// writable).
func (v *Versions) Writable() bool {
	if err := os.MkdirAll(v.dir(), 0o700); err != nil {
		return false
	}
	f, err := os.CreateTemp(v.dir(), ".probe-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

func readPointer(path string) (pointer, error) {
	var p pointer
	data, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(data, &p)
	return p, err
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// step is called before every step of Install (tests interrupt there).
var step = func(string) error { return nil }

// Install moves a verified download into the versions and makes it
// current; the current one becomes previous.
func (v *Versions) Install(path, ver string) error {
	name := "court-gateway-" + ver
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	final := v.file(name)
	sum, err := fileSHA256(path)
	if err != nil {
		return err
	}
	if err := step("place"); err != nil {
		return err
	}
	if err := os.Rename(path, final); err != nil {
		return err
	}
	if err := step("previous"); err != nil {
		return err
	}
	if data, err := os.ReadFile(v.file("current.json")); err == nil {
		if err := core.WriteFileAtomic(v.file("previous.json"), data, 0o600); err != nil {
			return err
		}
	} else if err := os.Remove(v.file("previous.json")); err != nil && !os.IsNotExist(err) {
		return err // the previous one is the installed binary
	}
	if err := step("current"); err != nil {
		return err
	}
	data, _ := json.Marshal(pointer{Version: ver, Path: final, SHA256: sum})
	if err := core.WriteFileAtomic(v.file("current.json"), data, 0o600); err != nil {
		return err
	}
	if err := step("prune"); err != nil {
		return err
	}
	v.prune()
	return nil
}

// prune removes version files that neither pointer names, and stray
// downloads.
func (v *Versions) prune() {
	keep := map[string]bool{}
	for _, f := range []string{"current.json", "previous.json"} {
		if p, err := readPointer(v.file(f)); err == nil {
			keep[p.Path] = true
		}
	}
	entries, _ := os.ReadDir(v.dir())
	for _, e := range entries {
		p := v.file(e.Name())
		if (strings.HasPrefix(e.Name(), "court-gateway-") || strings.HasPrefix(e.Name(), ".court-gateway-download-")) && !keep[p] {
			_ = os.Remove(p)
		}
	}
}

// Rollback makes the previous version current again, or, with none, goes
// back to the installed binary.
func (v *Versions) Rollback() error {
	if data, err := os.ReadFile(v.file("previous.json")); err == nil {
		if err := core.WriteFileAtomic(v.file("current.json"), data, 0o600); err != nil {
			return err
		}
		return os.Remove(v.file("previous.json"))
	}
	err := os.Remove(v.file("current.json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Launch returns the version the installed binary (version own) should
// hand over to: the current one, or else the previous one, that is newer
// than own, still has the checksum it was verified with and has not
// failed here recently. "" means run the installed binary itself.
func (v *Versions) Launch(own string, now time.Time) string {
	for _, f := range []string{"current.json", "previous.json"} {
		p, err := readPointer(v.file(f))
		if err != nil || !version.Newer(p.Version, own) || filepath.Dir(p.Path) != v.dir() || isBad(v.Dir, p.Version, now) {
			continue
		}
		if sum, err := fileSHA256(p.Path); err == nil && sum == p.SHA256 {
			return p.Path
		}
	}
	return ""
}

// LaunchedEnv is set on a version started by the installed binary, so it
// does not hand over again. InstalledEnv carries the installed binary's
// path into it.
const (
	LaunchedEnv  = "COURT_GATEWAY_LAUNCHED"
	InstalledEnv = "COURT_GATEWAY_INSTALLED"
)

// InstalledPath is the binary the service manager starts: the running
// executable, or, in a version it handed over to, the one it names.
func InstalledPath() string {
	if p := os.Getenv(InstalledEnv); p != "" {
		return p
	}
	exe, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return exe
}

// LaunchEnv is the environment of a version the installed binary hands
// over to.
func LaunchEnv(installed string) []string {
	return append(baseEnv(), LaunchedEnv+"=1", InstalledEnv+"="+installed)
}

// RestartEnv is the environment for starting the installed binary again,
// which then picks the version to run.
func RestartEnv() []string { return baseEnv() }

func baseEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, LaunchedEnv+"=") && !strings.HasPrefix(kv, InstalledEnv+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// SelfCheckTransientExit is the exit code of `selfcheck` when the
// environment (CRM, hub) rather than the binary failed.
const SelfCheckTransientExit = 10

// ChildCheck runs the downloaded binary before the switch: `version` must
// name the version from the signed manifest, and `selfcheck` with the live
// config must pass.
func ChildCheck(configPath string) func(ctx context.Context, path, ver string) error {
	return func(ctx context.Context, path, ver string) error {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		out, err := exec.CommandContext(cctx, path, "version").Output()
		if err != nil {
			return fmt.Errorf("%w: version: %v", ErrBroken, err)
		}
		f := strings.Fields(string(out))
		if len(f) < 2 || f[0] != "court-gateway" || f[1] != ver {
			return fmt.Errorf("%w: reports %q, manifest says %s", ErrBroken, firstLine(string(out)), ver)
		}
		cmd := exec.CommandContext(cctx, path, "selfcheck", "-config", configPath)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err = cmd.Run()
		var ee *exec.ExitError
		switch {
		case err == nil:
			return nil
		case errors.As(err, &ee) && ee.ExitCode() == SelfCheckTransientExit:
			return fmt.Errorf("%w: %s", ErrTransient, firstLine(stderr.String()))
		case cctx.Err() != nil:
			return fmt.Errorf("%w: self-check timed out", ErrTransient)
		default:
			return fmt.Errorf("%w: %s", ErrBroken, firstLine(stderr.String()))
		}
	}
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
