package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// HealthStatusFile is written by `serve` into the state directory and read
// by `healthcheck` when the gateway has no local port (the normal case with
// a hub). It holds no secrets and no CRM data.
const HealthStatusFile = "health.json"

// HealthStatus is the content of HealthStatusFile.
type HealthStatus struct {
	Time    time.Time `json:"time"`
	Version string    `json:"version"`
	Ready   bool      `json:"ready"`
	Reason  string    `json:"reason,omitempty"`
	// Hub is "connected", "disconnected" or empty without a hub.
	Hub string `json:"hub,omitempty"`
}

// WriteHealthStatus replaces the status file atomically.
func WriteHealthStatus(dir string, st HealthStatus) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return WriteFileAtomic(filepath.Join(dir, HealthStatusFile), data, 0o644)
}

// ReadHealthStatus reads the status file.
func ReadHealthStatus(dir string) (HealthStatus, error) {
	var st HealthStatus
	data, err := os.ReadFile(filepath.Join(dir, HealthStatusFile))
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("%s: %w", HealthStatusFile, err)
	}
	return st, nil
}

// WriteFileAtomic writes data to a temporary file in the same directory
// and renames it over path, so readers never see a partial file.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
