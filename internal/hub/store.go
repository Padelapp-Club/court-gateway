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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
)

// Layout of the state directory. Every record is its own small JSON file
// written atomically; nothing in it is a plaintext secret:
//
//	codes/<sha256 of the code>.json      pending join codes
//	gateways/<club>.json                 registered gateways (secret hash)
//
// Join codes are created by the `hub join-code create` command, possibly
// while the hub runs, so codes are read from disk at redemption time.
// Gateways are written only by the running hub.
const (
	codesDir    = "codes"
	gatewaysDir = "gateways"
)

// DefaultJoinCodeTTL is how long a join code stays valid.
const DefaultJoinCodeTTL = 24 * time.Hour

// ErrJoinCode is returned for an unknown, used or expired code alike.
var ErrJoinCode = errors.New("invalid, used or expired join code")

// GatewayRecord is a registered gateway.
type GatewayRecord struct {
	Club         string    `json:"club"`
	Name         string    `json:"name"`
	SecretSHA256 string    `json:"secret_sha256"`
	RegisteredAt time.Time `json:"registered_at"`
}

type joinCodeRecord struct {
	Club    string    `json:"club"`
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
}

// Store is the hub's state.
type Store struct {
	dir string
	now func() time.Time

	mu       sync.Mutex
	gateways map[string]*GatewayRecord
}

func ensureDirs(dir string) error {
	for _, d := range []string{dir, filepath.Join(dir, codesDir), filepath.Join(dir, gatewaysDir)} {
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
	s := &Store{dir: dir, now: now, gateways: map[string]*GatewayRecord{}}
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

// CreateJoinCode writes a new single-use code for club and returns it. The
// code itself is not stored, only its hash.
func CreateJoinCode(dir, club, name string, ttl time.Duration, now time.Time) (string, time.Time, error) {
	if !ValidClubID(club) {
		return "", time.Time{}, fmt.Errorf("--club: «%s» — строчные латинские буквы, цифры и дефис", club)
	}
	if strings.TrimSpace(name) == "" {
		name = club
	}
	if ttl <= 0 {
		ttl = DefaultJoinCodeTTL
	}
	if err := ensureDirs(dir); err != nil {
		return "", time.Time{}, err
	}
	code, err := newJoinCode()
	if err != nil {
		return "", time.Time{}, err
	}
	rec := joinCodeRecord{Club: club, Name: strings.TrimSpace(name), Created: now.UTC(), Expires: now.Add(ttl).UTC()}
	data, _ := json.Marshal(rec)
	if err := core.WriteFileAtomic(filepath.Join(dir, codesDir, codeHash(code)+".json"), data, 0o600); err != nil {
		return "", time.Time{}, err
	}
	return code, rec.Expires, nil
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
// of its club. The previous secret of that club stops working. The code
// file is removed before the secret is issued, so a code can never be used
// twice, even across a crash.
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
	if err := json.Unmarshal(data, &rec); err != nil || !ValidClubID(rec.Club) {
		return GatewayRecord{}, "", ErrJoinCode
	}
	if !s.now().Before(rec.Expires) {
		return GatewayRecord{}, "", ErrJoinCode
	}
	secret, err := newSecret()
	if err != nil {
		return GatewayRecord{}, "", err
	}
	g := &GatewayRecord{Club: rec.Club, Name: rec.Name, SecretSHA256: secretHash(secret), RegisteredAt: s.now().UTC()}
	out, _ := json.Marshal(g)
	if err := core.WriteFileAtomic(filepath.Join(s.dir, gatewaysDir, g.Club+".json"), out, 0o600); err != nil {
		return GatewayRecord{}, "", err
	}
	s.gateways[g.Club] = g
	return *g, secret, nil
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

// Get returns a registered gateway.
func (s *Store) Get(club string) (GatewayRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.gateways[club]
	if !ok {
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

// RevokeGateway removes a club's gateway registration on disk. A running
// hub stops accepting its secret at the next request.
func RevokeGateway(dir, club string) error {
	if !ValidClubID(club) {
		return fmt.Errorf("--club: «%s» — не идентификатор клуба", club)
	}
	err := os.Remove(filepath.Join(dir, gatewaysDir, club+".json"))
	if os.IsNotExist(err) {
		return fmt.Errorf("клуб %s не зарегистрирован", club)
	}
	return err
}
