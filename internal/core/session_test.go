package core

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The session survives the process: what one store saved, another store
// of the same directory loads; the file is the owner's only; nil removes.
func TestSessionStoreKeepsTheSessionForTheOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if b, err := SessionStore(dir).LoadSession(); b != nil || err != nil {
		t.Fatalf("nothing saved: %q %v", b, err)
	}
	if err := SessionStore(dir).SaveSession([]byte("session-secret")); err != nil {
		t.Fatal(err)
	}
	if b, err := SessionStore(dir).LoadSession(); err != nil || !bytes.Equal(b, []byte("session-secret")) {
		t.Fatalf("after a restart: %q %v", b, err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, SessionFile))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("session file mode = %v (%v), want 0600", fi.Mode().Perm(), err)
		}
		if di, _ := os.Stat(dir); di.Mode().Perm() != 0o700 {
			t.Fatalf("state dir mode = %v, want 0700", di.Mode().Perm())
		}
		// a file others can read is not used
		if err := os.Chmod(filepath.Join(dir, SessionFile), 0o644); err != nil {
			t.Fatal(err)
		}
		if b, err := SessionStore(dir).LoadSession(); err == nil || b != nil {
			t.Fatalf("a world-readable session was used: %q %v", b, err)
		}
	}
	if err := SessionStore(dir).SaveSession(nil); err != nil {
		t.Fatal(err)
	}
	if b, err := SessionStore(dir).LoadSession(); b != nil || err != nil {
		t.Fatalf("after removal: %q %v", b, err)
	}
}
