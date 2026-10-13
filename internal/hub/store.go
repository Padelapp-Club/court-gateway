package hub

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Padelapp-Club/court-gateway/internal/core"
)

// Layout of the state directory. Every record is its own small JSON file
// written atomically; nothing in it is a plaintext secret:
//
//	codes/<sha256 of the code>.json      pending join codes
//	gateways/<installation>.json         registered gateways (secret hash)
//	installations/<installation>.json    installations and their clubs' config
//
// One installation of the gateway is one CRM account, which may serve
// several clubs. Join codes bind to an installation. They are created by
// the `hub join-code create` command (possibly while the hub runs) or by
// the operator API, so codes are read from disk at redemption time.
// Gateways and installations are written only by the running hub (the
// `hub gateway revoke` command only removes a gateway file).
//
// Historical names: a gateway record and a join code of a hub from before
// installations say "club" for the installation id; such an installation
// serves the one club of the same id written in its gateway.yaml.
const (
	codesDir         = "codes"
	gatewaysDir      = "gateways"
	installationsDir = "installations"
)

// DefaultJoinCodeTTL is how long a join code stays valid.
const DefaultJoinCodeTTL = 24 * time.Hour

// ErrJoinCode is returned for an unknown, used or expired code alike.
var ErrJoinCode = errors.New("invalid, used or expired join code")

// GatewayRecord is a registered gateway. Club is the installation id (its
// historical name, kept on disk and in /hub/status).
type GatewayRecord struct {
	Club         string    `json:"club"`
	Name         string    `json:"name"`
	SecretSHA256 string    `json:"secret_sha256"`
	RegisteredAt time.Time `json:"registered_at"`
}

type joinCodeRecord struct {
	Installation string    `json:"installation,omitempty"`
	Club         string    `json:"club,omitempty"` // historical: the installation id
	Name         string    `json:"name,omitempty"`
	Created      time.Time `json:"created"`
	Expires      time.Time `json:"expires"`
}

func (r joinCodeRecord) installation() string {
	if r.Installation != "" {
		return r.Installation
	}
	return r.Club
}

// Installation is one installation of the gateway (one CRM account) and
// the configuration of the clubs it serves. Version grows with every
// change of the clubs.
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

// Store is the hub's state.
type Store struct {
	dir string
	now func() time.Time

	mu       sync.Mutex
	gateways map[string]*GatewayRecord
	installs map[string]*Installation
	clubs    map[string]string // club id -> installation id
}

func ensureDirs(dir string) error {
	for _, d := range []string{dir, filepath.Join(dir, codesDir), filepath.Join(dir, gatewaysDir), filepath.Join(dir, installationsDir)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// OpenStore loads the registered gateways from dir.
func OpenStore(dir string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	if err := ensureDirs(dir); err != nil {
		return nil, fmt.Errorf("state_dir: %w", err)
	}
	s := &Store{dir: dir, now: now, gateways: map[string]*GatewayRecord{}, installs: map[string]*Installation{}, clubs: map[string]string{}}
	if err := s.loadInstallations(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, gatewaysDir))
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var g GatewayRecord
		data, err := os.ReadFile(filepath.Join(dir, gatewaysDir, e.Name()))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &g); err != nil || !ValidClubID(g.Club) || len(g.SecretSHA256) != 64 {
			return nil, fmt.Errorf("state_dir: повреждённая запись шлюза %s", e.Name())
		}
		s.gateways[g.Club] = &g
	}
	return s, nil
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

// newIDRe is the rule for ids handed out from now on (installations and
// clubs): 2 to 63 characters. Loading keeps accepting every id ValidClubID
// accepts, so no existing registration is invalidated.
var newIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// ValidNewID reports whether id may name a new installation or club.
func ValidNewID(id string) bool { return newIDRe.MatchString(id) }

// Store errors.
var (
	ErrInvalidID           = errors.New("ids are 2 to 63 characters: lower-case latin letters, digits and dashes, starting with a letter or digit")
	ErrInvalidName         = errors.New("name must be at most 200 characters without control characters")
	ErrInvalidTTL          = errors.New("ttl must be positive and at most 720h")
	ErrUnknownInstallation = errors.New("no such installation")
	ErrInstallationExists  = errors.New("the installation already exists")
	ErrUnknownClub         = errors.New("no such club")
	ErrClubTaken           = errors.New("the club id is already served by another installation")
	ErrNotRegistered       = errors.New("no gateway is registered for this installation")
)

func validName(name string) bool {
	return utf8.ValidString(name) && utf8.RuneCountInString(name) <= maxName && strings.IndexFunc(name, unicode.IsControl) < 0
}

// JoinCode is a freshly created join code. Code is shown once: only its
// hash is stored.
type JoinCode struct {
	Code         string
	Installation string
	Expires      time.Time
}

// CreateJoinCode writes a new single-use code for an installation and
// returns it. The code itself is not stored, only its hash. A zero ttl
// means DefaultJoinCodeTTL. name names the installation if the code
// creates it at redemption (codes made by the CLI); an installation that
// already has a gateway may get a code: redeeming it replaces the
// gateway's secret (a reinstall or a replaced machine). Both the CLI and
// the operator API create codes here.
func CreateJoinCode(dir, installation, name string, ttl time.Duration, now time.Time) (JoinCode, error) {
	if !ValidNewID(installation) {
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
	if err := ensureDirs(dir); err != nil {
		return JoinCode{}, err
	}
	code, err := newJoinCode()
	if err != nil {
		return JoinCode{}, err
	}
	rec := joinCodeRecord{Installation: installation, Name: name, Created: now.UTC(), Expires: now.Add(ttl).UTC()}
	data, _ := json.Marshal(rec)
	if err := core.WriteFileAtomic(filepath.Join(dir, codesDir, codeHash(code)+".json"), data, 0o600); err != nil {
		return JoinCode{}, err
	}
	return JoinCode{Code: code, Installation: installation, Expires: rec.Expires}, nil
}

// CreateJoinCode is CreateJoinCode for the running hub: the installation
// must exist.
func (s *Store) CreateJoinCode(installation string, ttl time.Duration) (JoinCode, error) {
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
	return CreateJoinCode(s.dir, installation, name, ttl, s.now())
}

func newSecret() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "cgs_" + hex.EncodeToString(raw[:]), nil
}

func secretHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Redeem consumes a join code and registers (or re-registers) the gateway
// of its installation. The previous secret of that installation stops
// working. The code file is removed before the secret is issued, so a code
// can never be used twice, even across a crash. A code made by the CLI for
// an installation the hub does not know yet creates it.
func (s *Store) Redeem(code string) (GatewayRecord, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	norm := NormalizeJoinCode(code)
	if len(norm) != 16 || strings.Trim(norm, joinAlphabet) != "" {
		return GatewayRecord{}, "", ErrJoinCode
	}
	p := filepath.Join(s.dir, codesDir, codeHash(norm)+".json")
	data, err := os.ReadFile(p)
	if err != nil {
		return GatewayRecord{}, "", ErrJoinCode
	}
	if err := os.Remove(p); err != nil {
		return GatewayRecord{}, "", ErrJoinCode
	}
	var rec joinCodeRecord
	if err := json.Unmarshal(data, &rec); err != nil || !ValidClubID(rec.installation()) {
		return GatewayRecord{}, "", ErrJoinCode
	}
	if !s.now().Before(rec.Expires) {
		return GatewayRecord{}, "", ErrJoinCode
	}
	id := rec.installation()
	in, ok := s.installs[id]
	if !ok {
		in = &Installation{ID: id, Name: firstNonEmpty(rec.Name, id), Created: s.now().UTC(), Clubs: map[string]core.ClubSpec{}}
		if err := s.saveInstallation(in); err != nil {
			return GatewayRecord{}, "", err
		}
		s.installs[id] = in
	}
	secret, err := newSecret()
	if err != nil {
		return GatewayRecord{}, "", err
	}
	g := &GatewayRecord{Club: id, Name: in.Name, SecretSHA256: secretHash(secret), RegisteredAt: s.now().UTC()}
	out, _ := json.Marshal(g)
	if err := core.WriteFileAtomic(filepath.Join(s.dir, gatewaysDir, g.Club+".json"), out, 0o600); err != nil {
		return GatewayRecord{}, "", err
	}
	s.gateways[g.Club] = g
	return *g, secret, nil
}

func firstNonEmpty(v ...string) string {
	for _, x := range v {
		if strings.TrimSpace(x) != "" {
			return strings.TrimSpace(x)
		}
	}
	return ""
}

// Authenticate maps a gateway secret to its club. Every record is compared
// in constant time without an early exit. A record whose file has been
// removed (`hub gateway revoke`) no longer authenticates.
func (s *Store) Authenticate(secret string) (string, bool) {
	if secret == "" {
		return "", false
	}
	want, _ := hex.DecodeString(secretHash(secret))
	s.mu.Lock()
	var club string
	for _, g := range s.gateways {
		have, _ := hex.DecodeString(g.SecretSHA256)
		if subtle.ConstantTimeCompare(want, have) == 1 {
			club = g.Club
		}
	}
	s.mu.Unlock()
	if club == "" {
		return "", false
	}
	if _, err := os.Stat(filepath.Join(s.dir, gatewaysDir, club+".json")); err != nil {
		s.mu.Lock()
		delete(s.gateways, club)
		s.mu.Unlock()
		return "", false
	}
	return club, true
}

// Get returns a registered gateway. A record whose file has been removed
// (`hub gateway revoke`) is forgotten here, as in Authenticate.
func (s *Store) Get(club string) (GatewayRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.gateways[club]
	if !ok {
		return GatewayRecord{}, false
	}
	if _, err := os.Stat(filepath.Join(s.dir, gatewaysDir, club+".json")); err != nil {
		delete(s.gateways, club)
		return GatewayRecord{}, false
	}
	return *g, true
}

// Clubs returns the registered gateways sorted by club id.
func (s *Store) Clubs() []GatewayRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]GatewayRecord, 0, len(s.gateways))
	for _, g := range s.gateways {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Club < out[j].Club })
	return out
}

// RevokeGateway removes an installation's gateway registration on disk (the
// CLI). A running hub stops accepting its secret at the next request; the
// installation and its clubs stay, so a new join code restores service.
func RevokeGateway(dir, installation string) error {
	if !ValidClubID(installation) {
		return ErrNotRegistered
	}
	err := os.Remove(filepath.Join(dir, gatewaysDir, installation+".json"))
	if os.IsNotExist(err) {
		return ErrNotRegistered
	}
	return err
}

// ---- installations ----

func (s *Store) loadInstallations() error {
	entries, err := os.ReadDir(filepath.Join(s.dir, installationsDir))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, installationsDir, e.Name()))
		if err != nil {
			return err
		}
		var in Installation
		if err := json.Unmarshal(data, &in); err != nil || !ValidClubID(in.ID) {
			return fmt.Errorf("state_dir: повреждённая запись установки %s", e.Name())
		}
		if in.Clubs == nil {
			in.Clubs = map[string]core.ClubSpec{}
		}
		for id := range in.Clubs {
			if other, dup := s.clubs[id]; dup {
				return fmt.Errorf("state_dir: клуб %s записан в установках %s и %s", id, other, in.ID)
			}
			s.clubs[id] = in.ID
		}
		s.installs[in.ID] = &in
	}
	return nil
}

// saveInstallation writes in; s.mu must be held.
func (s *Store) saveInstallation(in *Installation) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return core.WriteFileAtomic(filepath.Join(s.dir, installationsDir, in.ID+".json"), data, 0o600)
}

// CreateInstallation adds an installation with no clubs.
func (s *Store) CreateInstallation(id, name string) (Installation, error) {
	name = strings.TrimSpace(name)
	switch {
	case !ValidNewID(id):
		return Installation{}, ErrInvalidID
	case !validName(name):
		return Installation{}, ErrInvalidName
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.installs[id]; ok {
		return Installation{}, ErrInstallationExists
	}
	in := &Installation{ID: id, Name: firstNonEmpty(name, id), Created: s.now().UTC(), Clubs: map[string]core.ClubSpec{}}
	if err := s.saveInstallation(in); err != nil {
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

// Installations returns every installation sorted by id.
func (s *Store) Installations() []Installation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Installation, 0, len(s.installs))
	for _, in := range s.installs {
		out = append(out, in.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// DeleteInstallation removes an installation entirely: its clubs, its
// gateway registration (the gateway gets 401 at its next request) and its
// pending join codes.
func (s *Store) DeleteInstallation(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.installs[id]
	if !ok {
		return ErrUnknownInstallation
	}
	if err := os.Remove(filepath.Join(s.dir, gatewaysDir, id+".json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	delete(s.gateways, id)
	if entries, err := os.ReadDir(filepath.Join(s.dir, codesDir)); err == nil {
		for _, e := range entries {
			p := filepath.Join(s.dir, codesDir, e.Name())
			var rec joinCodeRecord
			if data, err := os.ReadFile(p); err == nil && json.Unmarshal(data, &rec) == nil && rec.installation() == id {
				_ = os.Remove(p)
			}
		}
	}
	if err := os.Remove(filepath.Join(s.dir, installationsDir, id+".json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	for club := range in.Clubs {
		delete(s.clubs, club)
	}
	delete(s.installs, id)
	return nil
}

// PutClub stores (adds or replaces) a club of an installation; the spec
// must have passed core.ValidateClubSpec. It returns whether the club is
// new and the installation's new config version.
func (s *Store) PutClub(installation string, spec core.ClubSpec) (bool, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.installs[installation]
	if !ok {
		return false, 0, ErrUnknownInstallation
	}
	if other, taken := s.clubs[spec.ID]; taken && other != installation {
		return false, 0, ErrClubTaken
	}
	_, existed := in.Clubs[spec.ID]
	if !ValidNewID(spec.ID) && !existed {
		return false, 0, ErrInvalidID
	}
	next := in.clone()
	next.Clubs[spec.ID] = spec
	next.Version++
	if err := s.saveInstallation(&next); err != nil {
		return false, 0, err
	}
	*in = next
	s.clubs[spec.ID] = installation
	return !existed, in.Version, nil
}

// DeleteClub removes a club of an installation and returns the new
// config version.
func (s *Store) DeleteClub(installation, club string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	if err := s.saveInstallation(&next); err != nil {
		return 0, err
	}
	*in = next
	delete(s.clubs, club)
	return in.Version, nil
}

// ConfigVersion is an installation's config version (0 when it has no
// record: a gateway from before installations).
func (s *Store) ConfigVersion(installation string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if in, ok := s.installs[installation]; ok {
		return in.Version
	}
	return 0
}

// Route returns the installation that serves club. A club configured on
// the hub belongs to its installation; otherwise an installation with the
// club's id and no clubs on the hub serves it (a gateway that keeps its
// one club in gateway.yaml, as before installations).
func (s *Store) Route(club string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inst, ok := s.clubs[club]; ok {
		return inst, true
	}
	if in, ok := s.installs[club]; ok && len(in.Clubs) > 0 {
		return "", false
	}
	if _, ok := s.gateways[club]; ok {
		return club, true
	}
	return "", false
}

// ClubRef is a club as partners see it in the list.
type ClubRef struct {
	ID           string
	Name         string
	Installation string
}

// ClubRefs lists every club partners can reach: those configured on the
// hub and the gateway.yaml clubs of installations without hub clubs.
func (s *Store) ClubRefs() []ClubRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ClubRef
	for club, inst := range s.clubs {
		out = append(out, ClubRef{ID: club, Name: s.installs[inst].Clubs[club].Name, Installation: inst})
	}
	for id, g := range s.gateways {
		if _, configured := s.clubs[id]; configured {
			continue
		}
		if in, ok := s.installs[id]; ok && len(in.Clubs) > 0 {
			continue
		}
		out = append(out, ClubRef{ID: id, Name: g.Name, Installation: id})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
