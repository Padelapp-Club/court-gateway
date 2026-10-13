package core

import (
	"os"
	"sync"
)

// RotatingFile is a log file that is moved to <path>.1 when it grows past
// Max bytes, so a gateway left alone for years never fills the disk.
type RotatingFile struct {
	Path string
	Max  int64

	mu   sync.Mutex
	f    *os.File
	size int64
}

// Write implements io.Writer.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	if r.Max > 0 && r.size+int64(len(p)) > r.Max {
		r.f.Close()
		_ = os.Remove(r.Path + ".1")
		_ = os.Rename(r.Path, r.Path+".1")
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *RotatingFile) open() error {
	f, err := os.OpenFile(r.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, info.Size()
	return nil
}

// Close closes the file.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// TrimStartupLog empties f when it is a regular file larger than max. It is
// meant for the stderr file a service manager appends to (launchd), which
// nothing else rotates: it only receives a few lines per start, so cutting
// it at start keeps it bounded.
func TrimStartupLog(f *os.File, max int64) {
	if info, err := f.Stat(); err == nil && info.Mode().IsRegular() && info.Size() > max {
		_ = f.Truncate(0)
	}
}
