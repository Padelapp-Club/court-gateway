package hub

import (
	"bytes"
	"cmp"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/drivers"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// Layout of the state directory (FileState); codes and gateway secrets
// are kept only as hashes, the signing key is the one secret in it unless
// signing_key_file names another place:
//
//	codes/<sha256 of the code>.json      pending join codes
//	gateways/<installation>.json         registered gateways (secret hash)
//	installations/<installation>.json    installations and their clubs' config
//	hub-signing.key                      the hub's ed25519 signing key (seed, hex)
//
// Codes are created by `hub join-code create` (possibly while the hub
// runs) or the operator API, so they are read from the state at
// redemption. Gateways and installations are written only by the running
// hub (`hub gateway revoke` only removes a gateway registration).
const (
	codesDir         = "codes"
	gatewaysDir      = "gateways"
	installationsDir = "installations"
	// signingKeyFile is made on the hub's first start and never leaves it:
	// gateways pin its public half when they register (tunnel/sign.go).
	signingKeyFile = "hub-signing.key"
)

// DefaultJoinCodeTTL is how long a join code stays valid.
const DefaultJoinCodeTTL = 24 * time.Hour

// ErrJoinCode is returned for an unknown, used or expired code alike.
var ErrJoinCode = errors.New("invalid, used or expired join code")

// GatewayRecord is the registered gateway of an installation.
type GatewayRecord struct {
	Installation string    `json:"installation"`
	Name         string    `json:"name"`
	SecretSHA256 string    `json:"secret_sha256"`
	RegisteredAt time.Time `json:"registered_at"`
	// gen numbers this registration within the running hub, growing: a
	// replaced or removed registration is retired in the broker by it.
	gen uint64
}

// registration names this registration in what the hub signs for it.
func (g GatewayRecord) registration() string { return tunnel.RegistrationOf(g.SecretSHA256) }

type joinCodeRecord struct {
	Installation string    `json:"installation"`
	Name         string    `json:"name,omitempty"`
	Created      time.Time `json:"created"`
	Expires      time.Time `json:"expires"`
}

// Installation is one gateway installation (one CRM account) and its
// clubs' configuration; Version grows with every change of the clubs.
type Installation struct {
	ID      string                   `json:"id"`
	Name    string                   `json:"name"`
	Created time.Time                `json:"created"`
	Version int64                    `json:"version"`
	Clubs   map[string]core.ClubSpec `json:"clubs"`
}

func (in *Installation) clone() Installation {
	out := *in
	out.Clubs = make(map[string]core.ClubSpec, len(in.Clubs))
	for k, v := range in.Clubs {
		out.Clubs[k] = v
	}
	return out
}

// ClubList returns the clubs sorted by id.
func (in Installation) ClubList() []core.ClubSpec {
	out := make([]core.ClubSpec, 0, len(in.Clubs))
	for _, c := range in.Clubs {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Store is the hub's state: what State keeps, held in memory by the
// running hub.
type Store struct {
	st  State
	now func() time.Time
	key ed25519.PrivateKey

	mu sync.Mutex
	// epoch is tunnel.HeaderConfigEpoch: new at every start, and again
	// when a reload finds a config other than the one announced under
	// its version (lower, gone, or other clubs), so that no version
	// number is announced twice in one epoch with different clubs.
	epoch    string
	gateways map[string]*GatewayRecord
	installs map[string]*Installation
	clubs    map[string]string // club id -> installation id
	seq      uint64            // the last registration generation issued
	// retire, when set, ends a replaced or removed registration's polls
	// (Broker.Retire); it is called with s.mu held.
	retire func(installation string, below uint64)
	// stale: a write may have taken effect without the hub learning it,
	// or the state refused one over a change the hub had not seen; the
	// memory is not known to match the state until it is read again
	// (reload). Meanwhile no config is served and nothing is changed.
	// Every change checks it under s.mu, so none runs while a reload
	// reads the state.
	stale    bool
	reloadMu sync.Mutex // one reload at a time
	// deleted numbers the installations this hub deleted, by the count
	// of deletions (deletions) at that moment: a redemption that took
	// its code before the deletion does not bring the installation back.
	deleted   map[string]uint64
	deletions uint64
}

// OpenStore opens the state kept as files in dir, with the signing key
// in it.
func OpenStore(dir string, now func() time.Time) (*Store, error) {
	if err := ensureDirs(dir); err != nil {
		return nil, fmt.Errorf("state_dir: %w", err)
	}
	key, err := loadSigningKey(dir)
	if err != nil {
		return nil, err
	}
	return NewStore(FileState(dir), key, now)
}

// Open opens the state and the signing key cfg names.
func Open(cfg *Settings, now func() time.Time) (*Store, error) {
	key, err := SigningKey(cfg)
	if err != nil {
		return nil, err
	}
	st, err := OpenState(cfg)
	if err != nil {
		return nil, err
	}
	s, err := NewStore(st, key, now)
	if err != nil {
		st.Close()
	}
	return s, err
}

// OpenState opens the state cfg names: state_dir or the database.
func OpenState(cfg *Settings) (State, error) {
	if cfg.DatabaseURL != "" {
		return openPostgres(cfg.DatabaseURL)
	}
	return FileState(cfg.StateDir), nil
}

// NewStore loads the installations and the registered gateways of st,
// signing with key.
func NewStore(st State, key ed25519.PrivateKey, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	installs, gateways, err := st.load()
	if err != nil {
		return nil, err
	}
	s := &Store{st: st, now: now, key: key, epoch: rand.Text(), gateways: map[string]*GatewayRecord{}, installs: map[string]*Installation{}, clubs: map[string]string{}, deleted: map[string]uint64{}}
	if err := s.apply(installs, gateways); err != nil {
		return nil, err
	}
	return s, nil
}

// apply puts what was read from the state in the memory; s.mu must be
// held. A registration with the same secret keeps its generation; a
// replaced or removed one is retired. An installation gone from the
// state, or holding a lower version or other clubs under the same one
// (the database lost acknowledged commits), starts a new epoch.
func (s *Store) apply(installs []*Installation, gateways []GatewayRecord) error {
	byID := make(map[string]*Installation, len(installs))
	clubs := map[string]string{}
	for _, in := range installs {
		if in.Clubs == nil {
			in.Clubs = map[string]core.ClubSpec{}
		}
		for id := range in.Clubs {
			if other, dup := clubs[id]; dup {
				return fmt.Errorf("state_dir: клуб %s записан в установках %s и %s", id, other, in.ID)
			}
			clubs[id] = in.ID
		}
		byID[in.ID] = in
	}
	stored := make(map[string]GatewayRecord, len(gateways))
	for _, g := range gateways {
		stored[g.Installation] = g
	}
	for id, cur := range s.gateways {
		if g, ok := stored[id]; !ok || g.SecretSHA256 != cur.SecretSHA256 {
			s.forget(id, cur.gen)
		}
	}
	for _, g := range gateways {
		if _, ok := s.gateways[g.Installation]; !ok {
			g.gen = s.nextGen(g.Installation)
			s.gateways[g.Installation] = &g
		}
	}
	for id, old := range s.installs {
		if in, ok := byID[id]; !ok || in.Version < old.Version || in.Version == old.Version && !sameClubs(in, old) {
			s.epoch = rand.Text()
			break
		}
	}
	s.installs, s.clubs, s.stale = byID, clubs, false
	return nil
}

// sameClubs reports whether a and b configure the same clubs.
func sameClubs(a, b *Installation) bool {
	ja, erra := json.Marshal(a.ClubList())
	jb, errb := json.Marshal(b.ClubList())
	return erra == nil && errb == nil && bytes.Equal(ja, jb)
}

// fresh reads the state again when the memory is stale; an error is
// ErrStateUnavailable and leaves it stale.
func (s *Store) fresh() error {
	s.mu.Lock()
	stale := s.stale
	s.mu.Unlock()
	if !stale {
		return nil
	}
	return s.reload()
}

// reload reads the state again while the memory is stale, without s.mu
// held: no change runs meanwhile, as every one refuses a stale memory.
func (s *Store) reload() error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	s.mu.Lock()
	stale := s.stale
	s.mu.Unlock()
	if !stale {
		return nil
	}
	installs, gateways, err := s.st.load()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStateUnavailable, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.apply(installs, gateways); err != nil {
		return fmt.Errorf("%w: %v", ErrStateUnavailable, err)
	}
	return nil
}

// wrote notes the outcome of a write to the state begun with s.mu held:
// one that may have taken effect unseen, or that the state refused over a
// change this hub has not seen (ErrConflict, a club it knows elsewhere),
// makes the memory stale.
func (s *Store) wrote(err error) error {
	if errors.Is(err, errOutcomeUnknown) || errors.Is(err, ErrConflict) || errors.Is(err, ErrClubTaken) {
		s.stale = true
	}
	return err
}

// settle reads the state again after it refused a write over a change
// this hub had not seen, so that the next call works on the state as it
// is; this one fails all the same.
func (s *Store) settle(err error) error {
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrClubTaken) {
		_ = s.reload()
	}
	return err
}

func (s *Store) putInstallation(in *Installation, prev int64) error {
	return s.wrote(s.st.putInstallation(in, prev))
}

func (s *Store) deleteInstallation(id string, prev int64) error {
	return s.wrote(s.st.deleteInstallation(id, prev))
}

func (s *Store) putGateway(g *GatewayRecord) error {
	return s.wrote(s.st.putGateway(g))
}

// Close releases the state's connections.
func (s *Store) Close() { s.st.Close() }

// Ready reports whether the state can be read and written now and the
// memory matches it (a stale one is read again first).
func (s *Store) Ready() error {
	if err := s.fresh(); err != nil {
		return err
	}
	return s.st.ready()
}

// loadSigningKey reads the hub's signing key, making it on the first start.
func loadSigningKey(dir string) (ed25519.PrivateKey, error) {
	p := filepath.Join(dir, signingKeyFile)
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, fmt.Errorf("state_dir: %w", err)
		}
		_, err = f.WriteString(hex.EncodeToString(seed) + "\n")
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, fmt.Errorf("state_dir: %w", err)
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	if err != nil {
		return nil, fmt.Errorf("state_dir: %w", err)
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("state_dir: повреждён ключ подписи хаба %s", p)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// SigningKey reads the hub's signing key: signing_key_file, which the hub
// only reads, or the key in state_dir, made on the first start.
func SigningKey(cfg *Settings) (ed25519.PrivateKey, error) {
	if cfg.SigningKeyFile == "" {
		if err := ensureDirs(cfg.StateDir); err != nil {
			return nil, fmt.Errorf("state_dir: %w", err)
		}
		return loadSigningKey(cfg.StateDir)
	}
	p := cfg.SigningKeyFile
	fi, err := os.Stat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("signing_key_file: файла %s нет — создайте ключ один раз: court-gateway hub keygen -out %s", p, p)
	case err != nil:
		return nil, fmt.Errorf("signing_key_file: %w", err)
	case !fi.Mode().IsRegular():
		return nil, fmt.Errorf("signing_key_file: %s — не обычный файл", p)
	}
	if msg := keyFileExposure(p, fi); msg != "" {
		return nil, fmt.Errorf("signing_key_file: %s: %s", p, msg)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("signing_key_file: %w", err)
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("signing_key_file: повреждён ключ подписи хаба %s", p)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// WriteSigningKey makes a new signing key in the file p, which must not
// exist (`hub keygen`), in the format of state_dir/hub-signing.key, closed
// to others as SigningKey requires (createKeyFile) and checked so.
func WriteSigningKey(p string) (ed25519.PublicKey, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	f, err := createKeyFile(p)
	if err != nil {
		return nil, err
	}
	_, err = f.WriteString(hex.EncodeToString(seed) + "\n")
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		var fi os.FileInfo
		if fi, err = os.Stat(p); err == nil {
			if msg := keyFileExposure(p, fi); msg != "" {
				err = fmt.Errorf("%s: %s", p, msg)
			}
		}
	}
	if err != nil {
		_ = os.Remove(p)
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey), nil
}

// HubKey is the public half of the hub's signing key.
func (s *Store) HubKey() ed25519.PublicKey { return s.key.Public().(ed25519.PublicKey) }

// PublicKey reads the public half of the signing key cfg names, making
// the key in state_dir as the hub's first start would (for the command
// line).
func PublicKey(cfg *Settings) (ed25519.PublicKey, error) {
	k, err := SigningKey(cfg)
	if err != nil {
		return nil, err
	}
	return k.Public().(ed25519.PublicKey), nil
}

// joinAlphabet is Crockford's base32: no I, L, O, U, so a code read aloud
// or copied by hand survives.
const joinAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NormalizeJoinCode upper-cases a code, drops separators and maps the
// letters people confuse with digits.
func NormalizeJoinCode(code string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(code) {
		switch r {
		case '-', ' ', '\t':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func codeHash(code string) string {
	sum := sha256.Sum256([]byte("court-gateway-join:" + NormalizeJoinCode(code)))
	return hex.EncodeToString(sum[:])
}

func newJoinCode() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	var b strings.Builder
	for i, v := range raw {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(joinAlphabet[int(v)%len(joinAlphabet)])
	}
	return b.String(), nil
}

// MaxJoinCodeTTL caps how long a join code may stay valid.
const MaxJoinCodeTTL = 30 * 24 * time.Hour

// maxName caps an installation or club name, in characters.
const maxName = 200

// Store errors.
var (
	ErrInvalidID           = errors.New("ids are 2 to 63 characters: lower-case latin letters, digits and dashes, starting with a letter or digit")
	ErrInvalidName         = errors.New("name must be at most 200 characters without control characters")
	ErrInvalidTTL          = errors.New("ttl must be positive and at most 720h")
	ErrUnknownInstallation = errors.New("no such installation")
	ErrInstallationExists  = errors.New("the installation already exists")
	ErrUnknownClub         = errors.New("no such club")
	ErrClubTaken           = errors.New("the club id is already served by another installation")
	ErrStudioTaken         = errors.New("the club's CRM studio is already served by another installation")
	ErrRoomTaken           = errors.New("a court's CRM room is already served by another club")
	ErrNotRegistered       = errors.New("no gateway is registered for this installation")
	// ErrStateUnavailable: the state (its directory or its database)
	// cannot be read or written right now; nothing is forgotten because of
	// it. A change that failed so may have been written all the same: the
	// hub reads the state again before it serves or changes anything.
	ErrStateUnavailable = errors.New("the hub's state is unavailable")
	// ErrConflict: the state was changed meanwhile behind this hub (by
	// another hub, or by a write whose answer it lost); nothing was
	// written, the hub has read the state again and the request may be
	// sent again.
	ErrConflict = errors.New("the installation was changed meanwhile, retry")
	// errOutcomeUnknown: a write went to the state and no answer came
	// back; it may have taken effect.
	errOutcomeUnknown = errors.New("the write may have taken effect")
)

func validName(name string) bool {
	return utf8.ValidString(name) && utf8.RuneCountInString(name) <= maxName && strings.IndexFunc(name, unicode.IsControl) < 0
}

// JoinCode is a fresh join code, shown once: only its hash is stored.
type JoinCode struct {
	Code         string
	Installation string
	Expires      time.Time
}

// CreateJoinCode writes a new single-use code for an installation (the
// CLI and the operator API); ttl 0 is DefaultJoinCodeTTL. name names the
// installation a CLI code creates at redemption. Redeeming a code of an
// installation that has a gateway replaces its secret (a reinstall).
func CreateJoinCode(st State, installation, name string, ttl time.Duration, now time.Time) (JoinCode, error) {
	if !core.ValidID(installation) {
		return JoinCode{}, ErrInvalidID
	}
	name = strings.TrimSpace(name)
	if !validName(name) {
		return JoinCode{}, ErrInvalidName
	}
	if ttl == 0 {
		ttl = DefaultJoinCodeTTL
	}
	if ttl < 0 || ttl > MaxJoinCodeTTL {
		return JoinCode{}, ErrInvalidTTL
	}
	code, err := newJoinCode()
	if err != nil {
		return JoinCode{}, err
	}
	rec := joinCodeRecord{Installation: installation, Name: name, Created: now.UTC(), Expires: now.Add(ttl).UTC()}
	if err := st.putCode(codeHash(code), rec); err != nil {
		return JoinCode{}, err
	}
	return JoinCode{Code: code, Installation: installation, Expires: rec.Expires}, nil
}

// CreateJoinCode is CreateJoinCode for the running hub (the installation exists).
func (s *Store) CreateJoinCode(installation string, ttl time.Duration) (JoinCode, error) {
	if err := s.fresh(); err != nil {
		return JoinCode{}, err
	}
	s.mu.Lock()
	in, ok := s.installs[installation]
	var name string
	if ok {
		name = in.Name
	}
	s.mu.Unlock()
	if !ok {
		return JoinCode{}, ErrUnknownInstallation
	}
	return CreateJoinCode(s.st, installation, name, ttl, s.now())
}

func newSecret() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "cgs_" + hex.EncodeToString(raw[:]), nil
}

// Redeem consumes a join code and registers the gateway of its
// installation (creating one a CLI code names), retiring the previous
// secret. The code goes before the secret is issued: never used twice.
// It is taken without s.mu held (the state takes it atomically), so a
// slow state holds up only this call.
func (s *Store) Redeem(code string) (GatewayRecord, string, error) {
	norm := NormalizeJoinCode(code)
	if len(norm) != 16 || strings.Trim(norm, joinAlphabet) != "" {
		return GatewayRecord{}, "", ErrJoinCode
	}
	s.mu.Lock()
	since := s.deletions
	s.mu.Unlock()
	rec, err := s.st.takeCode(codeHash(norm))
	if err != nil {
		return GatewayRecord{}, "", err
	}
	if !core.ValidID(rec.Installation) {
		return GatewayRecord{}, "", ErrJoinCode
	}
	if !s.now().Before(rec.Expires) {
		return GatewayRecord{}, "", ErrJoinCode
	}
	if err := s.fresh(); err != nil {
		return GatewayRecord{}, "", err
	}
	g, secret, err := s.register(rec, since)
	if errors.Is(err, ErrConflict) {
		// the installation was made or deleted meanwhile behind this
		// hub: register with the state read again
		if err = s.reload(); err == nil {
			g, secret, err = s.register(rec, since)
		}
	}
	if errors.Is(err, ErrConflict) {
		err = fmt.Errorf("%w: %w", ErrStateUnavailable, err)
	}
	return g, secret, err
}

// register issues a new secret for the installation of the taken code
// rec, making the installation a CLI code names. An installation this hub
// deleted after deletions was since (the redemption began before; its
// code was taken before the deletion removed it) is not made again.
func (s *Store) register(rec joinCodeRecord, since uint64) (GatewayRecord, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stale {
		return GatewayRecord{}, "", ErrStateUnavailable
	}
	id := rec.Installation
	if s.deleted[id] > since {
		return GatewayRecord{}, "", ErrJoinCode
	}
	in, ok := s.installs[id]
	if !ok {
		in = &Installation{ID: id, Name: cmp.Or(rec.Name, id), Created: s.now().UTC(), Clubs: map[string]core.ClubSpec{}}
		if err := s.putInstallation(in, -1); err != nil {
			return GatewayRecord{}, "", err
		}
		s.installs[id] = in
	}
	secret, err := newSecret()
	if err != nil {
		return GatewayRecord{}, "", err
	}
	g := &GatewayRecord{Installation: id, Name: in.Name, SecretSHA256: tunnel.SecretSHA256(secret), RegisteredAt: s.now().UTC()}
	if err := s.putGateway(g); err != nil {
		return GatewayRecord{}, "", err
	}
	g.gen = s.nextGen(id)
	s.gateways[id] = g
	return *g, secret, nil
}

// nextGen issues a new registration generation and retires every older
// one of installation; s.mu must be held.
func (s *Store) nextGen(installation string) uint64 {
	s.seq++
	if s.retire != nil {
		s.retire(installation, s.seq)
	}
	return s.seq
}

// Authenticate maps a gateway secret to its registration, comparing every
// record in constant time. A revoked record (file removed) is
// ErrNotRegistered; ErrStateUnavailable means the check itself failed.
func (s *Store) Authenticate(secret string) (GatewayRecord, error) {
	if secret == "" {
		return GatewayRecord{}, ErrNotRegistered
	}
	want, _ := hex.DecodeString(tunnel.SecretSHA256(secret))
	s.mu.Lock()
	var inst string
	for _, g := range s.gateways {
		have, _ := hex.DecodeString(g.SecretSHA256)
		if subtle.ConstantTimeCompare(want, have) == 1 {
			inst = g.Installation
		}
	}
	s.mu.Unlock()
	if inst == "" {
		return GatewayRecord{}, ErrNotRegistered
	}
	g, err := s.Gateway(inst)
	if err == nil && g.SecretSHA256 != tunnel.SecretSHA256(secret) {
		return GatewayRecord{}, ErrNotRegistered // re-registered meanwhile
	}
	return g, err
}

// Gateway returns the registered gateway of an installation as the state
// holds it: a revoked one is forgotten and its polls retired
// (ErrNotRegistered); one whose secret was replaced behind this hub (by
// another hub, or by a write whose answer it lost) takes the place of the
// one in memory, whose polls are retired. A failed check is
// ErrStateUnavailable and forgets nothing. The state is asked without
// s.mu held: a slow database holds up only this call.
func (s *Store) Gateway(installation string) (GatewayRecord, error) {
	if err := s.fresh(); err != nil {
		return GatewayRecord{}, err
	}
	s.mu.Lock()
	g, ok := s.gateways[installation]
	s.mu.Unlock()
	if !ok {
		return GatewayRecord{}, ErrNotRegistered
	}
	stored, err := s.st.gateway(installation)
	if err != nil {
		return GatewayRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case stored == nil:
		s.forget(installation, g.gen)
	case stored.SecretSHA256 != g.SecretSHA256:
		s.replace(installation, g.gen, *stored)
	}
	if cur, ok := s.gateways[installation]; ok {
		return *cur, nil // the same, the stored one, or registered meanwhile by this hub
	}
	return GatewayRecord{}, ErrNotRegistered
}

// replace puts the stored registration g in place of the registration gen
// of installation and retires its polls, unless a newer one replaced it
// meanwhile; s.mu must be held.
func (s *Store) replace(installation string, gen uint64, g GatewayRecord) {
	if cur, ok := s.gateways[installation]; ok && cur.gen == gen {
		g.gen = s.nextGen(installation)
		s.gateways[installation] = &g
	}
}

// forget drops the registration gen of installation, found revoked, and
// retires its polls, unless a newer one replaced it meanwhile; s.mu must
// be held.
func (s *Store) forget(installation string, gen uint64) {
	if cur, ok := s.gateways[installation]; ok && cur.gen == gen {
		delete(s.gateways, installation)
		s.nextGen(installation)
	}
}

// Gateways returns the registered gateways sorted by installation id,
// forgetting the revoked ones and taking the replaced ones from the state
// as Gateway does; when the state cannot be read, none is forgotten.
func (s *Store) Gateways() []GatewayRecord {
	s.mu.Lock()
	known := make([]GatewayRecord, 0, len(s.gateways))
	for _, g := range s.gateways {
		known = append(known, *g)
	}
	s.mu.Unlock()
	there, err := s.st.gateways()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]GatewayRecord, 0, len(known))
	for _, g := range known {
		if err == nil {
			switch stored, ok := there[g.Installation]; {
			case !ok:
				s.forget(g.Installation, g.gen)
				continue
			case stored.SecretSHA256 != g.SecretSHA256:
				s.replace(g.Installation, g.gen, stored)
			}
		}
		if cur, ok := s.gateways[g.Installation]; ok {
			out = append(out, *cur)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Installation < out[j].Installation })
	return out
}

// RevokeGateway removes an installation's gateway registration from the
// state (the CLI); the installation and its clubs stay for a new join code.
func RevokeGateway(st State, installation string) error {
	if !core.ValidID(installation) {
		return ErrNotRegistered
	}
	return st.deleteGateway(installation)
}

// ---- installations ----

// CreateInstallation adds an installation with no clubs.
func (s *Store) CreateInstallation(id, name string) (Installation, error) {
	name = strings.TrimSpace(name)
	switch {
	case !core.ValidID(id):
		return Installation{}, ErrInvalidID
	case !validName(name):
		return Installation{}, ErrInvalidName
	}
	if err := s.fresh(); err != nil {
		return Installation{}, err
	}
	in, err := s.createInstallation(id, name)
	if errors.Is(s.settle(err), ErrConflict) {
		err = ErrInstallationExists // made meanwhile behind this hub
	}
	return in, err
}

func (s *Store) createInstallation(id, name string) (Installation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stale {
		return Installation{}, ErrStateUnavailable
	}
	if _, ok := s.installs[id]; ok {
		return Installation{}, ErrInstallationExists
	}
	in := &Installation{ID: id, Name: cmp.Or(name, id), Created: s.now().UTC(), Clubs: map[string]core.ClubSpec{}}
	if err := s.putInstallation(in, -1); err != nil {
		return Installation{}, err
	}
	s.installs[id] = in
	return in.clone(), nil
}

// Installation returns one installation.
func (s *Store) Installation(id string) (Installation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.installs[id]
	if !ok {
		return Installation{}, false
	}
	return in.clone(), true
}

// DeleteInstallation removes an installation with its clubs, gateway
// registration (401 at the next request) and pending join codes.
func (s *Store) DeleteInstallation(id string) error {
	if err := s.fresh(); err != nil {
		return err
	}
	return s.settle(s.deleteInstallationLocked(id))
}

func (s *Store) deleteInstallationLocked(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stale {
		return ErrStateUnavailable
	}
	in, ok := s.installs[id]
	if !ok {
		return ErrUnknownInstallation
	}
	err := s.deleteInstallation(id, in.Version)
	if err == nil || errors.Is(err, errOutcomeUnknown) {
		// a deletion that may have taken effect counts as one for the
		// redemptions begun before it (at worst one is refused while the
		// installation stays)
		s.deletions++
		s.deleted[id] = s.deletions
	}
	if err != nil {
		return err
	}
	delete(s.gateways, id)
	s.nextGen(id)
	for club := range in.Clubs {
		delete(s.clubs, club)
	}
	delete(s.installs, id)
	return nil
}

// PutClub adds or replaces a club (validated by core.ValidateClubSpec) and
// returns whether it is new and the installation's new config version.
func (s *Store) PutClub(installation string, spec core.ClubSpec) (bool, int64, error) {
	if !core.ValidID(spec.ID) {
		return false, 0, ErrInvalidID
	}
	if err := s.fresh(); err != nil {
		return false, 0, err
	}
	created, v, err := s.putClub(installation, spec)
	return created, v, s.settle(err)
}

func (s *Store) putClub(installation string, spec core.ClubSpec) (bool, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stale {
		return false, 0, ErrStateUnavailable
	}
	in, ok := s.installs[installation]
	if !ok {
		return false, 0, ErrUnknownInstallation
	}
	if other, taken := s.clubs[spec.ID]; taken && other != installation {
		return false, 0, ErrClubTaken
	}
	studios, rooms := clubResources(spec)
	overlap := func(a, b []string) bool {
		return slices.ContainsFunc(a, func(v string) bool { return slices.Contains(b, v) })
	}
	for id, other := range s.installs {
		for cid, c := range other.Clubs {
			cs, cr := clubResources(c)
			switch {
			case id == installation && cid == spec.ID: // the club it replaces
			case id != installation && overlap(cs, studios):
				return false, 0, ErrStudioTaken
			case overlap(cr, rooms):
				return false, 0, ErrRoomTaken
			}
		}
	}
	_, existed := in.Clubs[spec.ID]
	next := in.clone()
	next.Clubs[spec.ID] = spec
	next.Version++
	if err := s.putInstallation(&next, in.Version); err != nil {
		return false, 0, err
	}
	*in = next
	s.clubs[spec.ID] = installation
	return !existed, in.Version, nil
}

// clubResources are the CRM studios a club reads and the rooms its courts
// book, normalized, by the drivers' keys (drivers.ResourceKeys).
func clubResources(c core.ClubSpec) (studios, rooms []string) {
	studioKeys, roomKeys := drivers.ResourceKeys()
	add := func(out *[]string, params map[string]any, keys []string) {
		for _, k := range keys {
			if v, _ := params[k].(string); strings.TrimSpace(v) != "" {
				*out = append(*out, k+":"+strings.ToLower(strings.TrimSpace(v)))
			}
		}
	}
	add(&studios, c.CRM, studioKeys)
	for _, ct := range c.Courts {
		add(&rooms, ct.Params, roomKeys)
	}
	return studios, rooms
}

// DeleteClub removes a club and returns the new config version.
func (s *Store) DeleteClub(installation, club string) (int64, error) {
	if err := s.fresh(); err != nil {
		return 0, err
	}
	v, err := s.deleteClub(installation, club)
	return v, s.settle(err)
}

func (s *Store) deleteClub(installation, club string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stale {
		return 0, ErrStateUnavailable
	}
	in, ok := s.installs[installation]
	if !ok {
		return 0, ErrUnknownInstallation
	}
	if _, ok := in.Clubs[club]; !ok {
		return 0, ErrUnknownClub
	}
	next := in.clone()
	delete(next.Clubs, club)
	next.Version++
	if err := s.putInstallation(&next, in.Version); err != nil {
		return 0, err
	}
	*in = next
	delete(s.clubs, club)
	return in.Version, nil
}

// served returns the installation whose config the hub signs for its
// gateway, with the epoch it is announced in: ErrStateUnavailable while
// the memory is stale.
func (s *Store) served(id string) (Installation, bool, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stale {
		return Installation{}, false, "", ErrStateUnavailable
	}
	in, ok := s.installs[id]
	if !ok {
		return Installation{}, false, s.epoch, nil
	}
	return in.clone(), true, s.epoch, nil
}

// announced is an installation's config version (0 when unknown) and the
// epoch it belongs to.
func (s *Store) announced(id string) (int64, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if in, ok := s.installs[id]; ok {
		return in.Version, s.epoch
	}
	return 0, s.epoch
}

// ConfigVersion is an installation's config version (0 when unknown).
func (s *Store) ConfigVersion(installation string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if in, ok := s.installs[installation]; ok {
		return in.Version
	}
	return 0
}

// Route returns the installation that serves club.
func (s *Store) Route(club string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.clubs[club]
	return inst, ok
}

// ClubRef is a club as partners see it in the list.
type ClubRef struct {
	ID           string
	Name         string
	Installation string
}

// ClubRefs lists every club configured on the hub, sorted by id.
func (s *Store) ClubRefs() []ClubRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ClubRef, 0, len(s.clubs))
	for club, inst := range s.clubs {
		out = append(out, ClubRef{ID: club, Name: s.installs[inst].Clubs[club].Name, Installation: inst})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
