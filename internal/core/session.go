package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// SessionFile is where an interactive account keeps its CRM session
// (driver.SessionStore), in the state directory. It is a secret of the
// same class as CRM_PASSWORD in .env: the login, not CRM data.
const SessionFile = "crm-session"

// SessionStore is the driver.SessionStore in the state directory dir.
type SessionStore string

var (
	_ driver.SessionStore = SessionStore("")
	_ driver.SessionStore = NamedSessionStore{}
)

// LoadSession implements driver.SessionStore. A session file anyone but
// the owner can read is refused, not used: it may have been read already.
func (d SessionStore) LoadSession() ([]byte, error) {
	return NamedSessionStore{Dir: string(d), File: SessionFile}.LoadSession()
}

// SaveSession implements driver.SessionStore: the file is replaced
// atomically with mode 0600 in a 0700 directory; nil removes it.
func (d SessionStore) SaveSession(b []byte) error {
	return NamedSessionStore{Dir: string(d), File: SessionFile}.SaveSession(b)
}

// NamedSessionStore is a driver.SessionStore in file File of the state
// directory Dir: the mirror's CRM session (MirrorSessionFile) is kept
// apart from the primary's, as SessionStore keeps that.
type NamedSessionStore struct {
	Dir, File string
}

// LoadSession implements driver.SessionStore, as SessionStore does.
func (n NamedSessionStore) LoadSession() ([]byte, error) {
	p := filepath.Join(n.Dir, n.File)
	fi, err := os.Stat(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s: файл сессии CRM доступен не только владельцу (%v) — войдите заново: court-gateway login", p, fi.Mode().Perm())
	}
	return os.ReadFile(p)
}

// SaveSession implements driver.SessionStore, as SessionStore does.
func (n NamedSessionStore) SaveSession(b []byte) error {
	p := filepath.Join(n.Dir, n.File)
	if b == nil {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(n.Dir, 0o700); err != nil {
		return err
	}
	return WriteFileAtomic(p, b, 0o600)
}
