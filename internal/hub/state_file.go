package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Padelapp-Club/court-gateway/internal/core"
)

// State is where the hub keeps what it must not forget: the files of
// state_dir (FileState) or a Postgres database (database_url_env). The
// command line writes join codes and revocations straight to it, and the
// running hub reads them there; everything else only the running hub
// writes. Codes and gateway secrets are kept only as hashes.
type State interface {
	// load reads the installations and the registered gateways.
	load() ([]*Installation, []GatewayRecord, error)
	// putCode stores a join code by the hash of the code.
	putCode(hash string, rec joinCodeRecord) error
	// takeCode removes the code with hash and returns it: of two takes of
	// one code exactly one gets it. A code that is not there is
	// ErrJoinCode.
	takeCode(hash string) (joinCodeRecord, error)
	// putGateway stores the gateway registration of its installation,
	// replacing the previous one.
	putGateway(g *GatewayRecord) error
	// hasGateway reports whether installation has a registration; an error
	// is ErrStateUnavailable and says nothing about it.
	hasGateway(installation string) (bool, error)
	// gateways returns the installations that have a registration.
	gateways() (map[string]bool, error)
	// deleteGateway removes the registration of installation
	// (ErrNotRegistered when there is none).
	deleteGateway(installation string) error
	// putInstallation stores in with its clubs, replacing the previous
	// version of it; it may raise in.Version to keep it above the stored
	// one.
	putInstallation(in *Installation) error
	// deleteInstallation removes an installation with its clubs, its
	// gateway registration and its pending join codes.
	deleteInstallation(id string) error
	// ready reports whether the state can be read and written now.
	ready() error
	// Close releases the state's connections.
	Close()
}

// FileState is the state kept as files in dir, each record its own small
// file written atomically:
//
//	codes/<sha256 of the code>.json      pending join codes
//	gateways/<installation>.json         registered gateways (secret hash)
//	installations/<installation>.json    installations and their clubs' config
func FileState(dir string) State { return fileState{dir: dir} }

type fileState struct{ dir string }

func ensureDirs(dir string) error {
	for _, d := range []string{dir, filepath.Join(dir, codesDir), filepath.Join(dir, gatewaysDir), filepath.Join(dir, installationsDir)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (f fileState) load() ([]*Installation, []GatewayRecord, error) {
	if err := ensureDirs(f.dir); err != nil {
		return nil, nil, fmt.Errorf("state_dir: %w", err)
	}
	installs, err := f.loadInstallations()
	if err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(filepath.Join(f.dir, gatewaysDir))
	if err != nil {
		return nil, nil, err
	}
	var gateways []GatewayRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var g GatewayRecord
		data, err := os.ReadFile(filepath.Join(f.dir, gatewaysDir, e.Name()))
		if err != nil {
			return nil, nil, err
		}
		if err := json.Unmarshal(data, &g); err != nil || !core.ValidID(g.Installation) || len(g.SecretSHA256) != 64 {
			return nil, nil, fmt.Errorf("state_dir: повреждённая запись шлюза %s", e.Name())
		}
		gateways = append(gateways, g)
	}
	return installs, gateways, nil
}

func (f fileState) loadInstallations() ([]*Installation, error) {
	entries, err := os.ReadDir(filepath.Join(f.dir, installationsDir))
	if err != nil {
		return nil, err
	}
	var out []*Installation
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(f.dir, installationsDir, e.Name()))
		if err != nil {
			return nil, err
		}
		var in Installation
		if err := json.Unmarshal(data, &in); err != nil || !core.ValidID(in.ID) {
			return nil, fmt.Errorf("state_dir: повреждённая запись установки %s", e.Name())
		}
		out = append(out, &in)
	}
	return out, nil
}

func (f fileState) putCode(hash string, rec joinCodeRecord) error {
	if err := ensureDirs(f.dir); err != nil {
		return err
	}
	data, _ := json.Marshal(rec)
	return core.WriteFileAtomic(filepath.Join(f.dir, codesDir, hash+".json"), data, 0o600)
}

// takeCode: the code file goes before the secret is issued, and of two
// removals of one file only one succeeds.
func (f fileState) takeCode(hash string) (joinCodeRecord, error) {
	p := filepath.Join(f.dir, codesDir, hash+".json")
	data, err := os.ReadFile(p)
	if err != nil {
		return joinCodeRecord{}, ErrJoinCode
	}
	if err := os.Remove(p); err != nil {
		return joinCodeRecord{}, ErrJoinCode
	}
	var rec joinCodeRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return joinCodeRecord{}, ErrJoinCode
	}
	return rec, nil
}

func (f fileState) putGateway(g *GatewayRecord) error {
	out, _ := json.Marshal(g)
	return core.WriteFileAtomic(filepath.Join(f.dir, gatewaysDir, g.Installation+".json"), out, 0o600)
}

func (f fileState) hasGateway(installation string) (bool, error) {
	_, err := os.Stat(filepath.Join(f.dir, gatewaysDir, installation+".json"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("%w: %v", ErrStateUnavailable, err)
	}
	return true, nil
}

func (f fileState) gateways() (map[string]bool, error) {
	entries, err := os.ReadDir(filepath.Join(f.dir, gatewaysDir))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStateUnavailable, err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".json"); ok && !e.IsDir() {
			out[id] = true
		}
	}
	return out, nil
}

func (f fileState) deleteGateway(installation string) error {
	err := os.Remove(filepath.Join(f.dir, gatewaysDir, installation+".json"))
	if os.IsNotExist(err) {
		return ErrNotRegistered
	}
	return err
}

func (f fileState) putInstallation(in *Installation) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return core.WriteFileAtomic(filepath.Join(f.dir, installationsDir, in.ID+".json"), data, 0o600)
}

func (f fileState) deleteInstallation(id string) error {
	if err := f.deleteGateway(id); err != nil && !errors.Is(err, ErrNotRegistered) {
		return err
	}
	if entries, err := os.ReadDir(filepath.Join(f.dir, codesDir)); err == nil {
		for _, e := range entries {
			p := filepath.Join(f.dir, codesDir, e.Name())
			var rec joinCodeRecord
			if data, err := os.ReadFile(p); err == nil && json.Unmarshal(data, &rec) == nil && rec.Installation == id {
				_ = os.Remove(p)
			}
		}
	}
	if err := os.Remove(filepath.Join(f.dir, installationsDir, id+".json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (fileState) ready() error { return nil }

func (fileState) Close() {}
