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

// Installer switches binaries.
type Installer interface {
	// StagingDir is where downloads go (same filesystem as the target, so
	// the final step is an atomic rename).
	StagingDir() string
	// Install moves the verified file into place and returns the binary
	// to execute next.
	Install(path, version string) (string, error)
	// Rollback restores the previous binary.
	Rollback() error
	// Current is the binary to execute now.
	Current() string
}

// InPlace replaces the running executable, keeping the previous one next
// to it as <exe>.prev. Renaming a running executable is allowed on Linux,
// macOS and Windows; the running process keeps its old image.
type InPlace struct {
	Exe string
}

// StagingDir implements Installer.
func (i *InPlace) StagingDir() string { return filepath.Dir(i.Exe) }

// Current implements Installer.
func (i *InPlace) Current() string { return i.Exe }

// Install implements Installer.
func (i *InPlace) Install(path, _ string) (string, error) {
	prev := i.Exe + ".prev"
	_ = os.Remove(prev)
	if err := os.Rename(i.Exe, prev); err != nil {
		return "", err
	}
	if err := os.Rename(path, i.Exe); err != nil {
		_ = os.Rename(prev, i.Exe)
		return "", err
	}
	return i.Exe, nil
}

// Rollback implements Installer.
func (i *InPlace) Rollback() error {
	prev := i.Exe + ".prev"
	if _, err := os.Stat(prev); err != nil {
		return fmt.Errorf("no previous binary to roll back to: %w", err)
	}
	failed := i.Exe + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(i.Exe, failed); err != nil {
		return err
	}
	if err := os.Rename(prev, i.Exe); err != nil {
		_ = os.Rename(failed, i.Exe)
		return err
	}
	return nil
}

// CacheDir keeps updated binaries in a writable directory when the shipped
// executable is read-only (the container image): <dir>/bin holds
// court-gateway-<version> files and current.json points at the one to run.
// At start the shipped binary calls Launch, which runs the newest verified
// cached binary, and falls back to itself when there is none.
type CacheDir struct {
	Dir string
	// Shipped is the read-only binary that came with the installation.
	Shipped string
}

type pointer struct {
	Version string `json:"version"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
}

func (c *CacheDir) bin() string { return filepath.Join(c.Dir, "bin") }

// StagingDir implements Installer.
func (c *CacheDir) StagingDir() string { return c.bin() }

// Current implements Installer.
func (c *CacheDir) Current() string {
	if p, err := readPointer(filepath.Join(c.bin(), "current.json")); err == nil {
		return p.Path
	}
	return c.Shipped
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

// Install implements Installer.
func (c *CacheDir) Install(path, ver string) (string, error) {
	name := "court-gateway-" + ver
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	final := filepath.Join(c.bin(), name)
	sum, err := fileSHA256(path)
	if err != nil {
		return "", err
	}
	if err := os.Rename(path, final); err != nil {
		return "", err
	}
	cur := filepath.Join(c.bin(), "current.json")
	if data, err := os.ReadFile(cur); err == nil {
		_ = core.WriteFileAtomic(filepath.Join(c.bin(), "previous.json"), data, 0o600)
	} else {
		_ = os.Remove(filepath.Join(c.bin(), "previous.json")) // previous is the shipped binary
	}
	data, _ := json.Marshal(pointer{Version: ver, Path: final, SHA256: sum})
	if err := core.WriteFileAtomic(cur, data, 0o600); err != nil {
		return "", err
	}
	c.prune(final)
	return final, nil
}

// prune removes cached binaries other than the current and previous ones.
func (c *CacheDir) prune(keep string) {
	prev, _ := readPointer(filepath.Join(c.bin(), "previous.json"))
	entries, _ := os.ReadDir(c.bin())
	for _, e := range entries {
		p := filepath.Join(c.bin(), e.Name())
		if strings.HasPrefix(e.Name(), "court-gateway-") && p != keep && p != prev.Path {
			_ = os.Remove(p)
		}
	}
}

// Rollback implements Installer.
func (c *CacheDir) Rollback() error {
	cur := filepath.Join(c.bin(), "current.json")
	prev := filepath.Join(c.bin(), "previous.json")
	if data, err := os.ReadFile(prev); err == nil {
		if err := core.WriteFileAtomic(cur, data, 0o600); err != nil {
			return err
		}
		return os.Remove(prev)
	}
	err := os.Remove(cur) // back to the shipped binary
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// LaunchedEnv is set on a process started by Launch, so it never launches
// again.
const LaunchedEnv = "COURT_GATEWAY_LAUNCHED"

// Launch returns the cached binary the shipped binary should hand over to:
// newer than own, unchanged since it was verified, not marked bad. "" means
// run the shipped binary.
func (c *CacheDir) Launch(own string, now time.Time) string {
	if os.Getenv(LaunchedEnv) != "" {
		return ""
	}
	p, err := readPointer(filepath.Join(c.bin(), "current.json"))
	if err != nil || !version.Newer(p.Version, own) {
		return ""
	}
	if filepath.Dir(p.Path) != c.bin() {
		return ""
	}
	if t, bad := BadVersions(c.Dir)[p.Version]; bad && now.Sub(t) < BadVersionTTL {
		return ""
	}
	sum, err := fileSHA256(p.Path)
	if err != nil || sum != p.SHA256 {
		return ""
	}
	return p.Path
}

// ChooseInstaller replaces the executable in place when its directory is
// writable, and otherwise caches updates in stateDir. ok is false when
// neither works (updates stay off and the hub shows the old version).
func ChooseInstaller(exe, stateDir string) (Installer, bool) {
	if f, err := os.CreateTemp(filepath.Dir(exe), ".court-gateway-probe-*"); err == nil {
		name := f.Name()
		f.Close()
		os.Remove(name)
		return &InPlace{Exe: exe}, true
	}
	if runtime.GOOS == "windows" {
		return nil, false
	}
	c := &CacheDir{Dir: stateDir, Shipped: exe}
	if err := os.MkdirAll(c.bin(), 0o700); err != nil {
		return nil, false
	}
	return c, true
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
		if got := strings.TrimSpace(string(out)); !strings.HasPrefix(got, "court-gateway "+ver) {
			return fmt.Errorf("%w: reports %q, manifest says %s", ErrBroken, firstLine(got), ver)
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
