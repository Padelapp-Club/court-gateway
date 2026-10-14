package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Platforms are the release builds.
var Platforms = []string{"linux-amd64", "linux-arm64", "darwin-arm64", "darwin-amd64", "windows-amd64", "windows-arm64"}

// AssetName is the file name of a release build.
func AssetName(ver, platform string) string {
	name := "court-gateway-" + ver + "-" + platform
	if strings.HasPrefix(platform, "windows-") {
		name += ".exe"
	}
	return name
}

// GenerateKey returns a new signing key pair: the private key file content
// (base64 seed) and the base64 public key to embed into release builds.
func GenerateKey() (privFile []byte, pub string, err error) {
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	return []byte(base64.StdEncoding.EncodeToString(sk.Seed()) + "\n"), base64.StdEncoding.EncodeToString(pk), nil
}

// ParsePrivateKey decodes a private key file.
func ParsePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("private key file is not a base64 ed25519 seed")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// BuildManifest hashes the release builds of ver found in dir.
func BuildManifest(ver, dir, baseURL string, released time.Time) ([]byte, error) {
	m := Manifest{Version: ver, Released: released.UTC().Format(time.RFC3339), Assets: map[string]Asset{}}
	for _, p := range Platforms {
		name := AssetName(ver, p)
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("missing build %s: %w", name, err)
		}
		sum := sha256.Sum256(data)
		m.Assets[p] = Asset{URL: strings.TrimRight(baseURL, "/") + "/" + name, SHA256: hex.EncodeToString(sum[:])}
	}
	return json.MarshalIndent(m, "", "  ")
}

// SumsNamespace is the namespace of the SSH signature of SHA256SUMS.
const SumsNamespace = "court-gateway"

// SignSums returns SHA256SUMS.sig: the SSH signature of sums (the format
// of ssh-keygen -Y sign, namespace SumsNamespace). A first install checks
// it with ssh-keygen -Y verify against the release key the install docs
// publish (AllowedSigner), before it trusts any program it downloaded.
func SignSums(priv ed25519.PrivateKey, sums []byte) []byte {
	hash := sha512.Sum512(sums)
	fields := slices.Concat(str([]byte(SumsNamespace)), str(nil), str([]byte("sha512")))
	sig := ed25519.Sign(priv, slices.Concat([]byte("SSHSIG"), fields, str(hash[:])))
	pub := slices.Concat(str([]byte("ssh-ed25519")), str(priv.Public().(ed25519.PublicKey)))
	blob := slices.Concat([]byte("SSHSIG"), binary.BigEndian.AppendUint32(nil, 1), str(pub), fields,
		str(slices.Concat(str([]byte("ssh-ed25519")), str(sig))))
	b64 := base64.StdEncoding.EncodeToString(blob)
	out := "-----BEGIN SSH SIGNATURE-----\n"
	for ; len(b64) > 70; b64 = b64[70:] {
		out += b64[:70] + "\n"
	}
	return []byte(out + b64 + "\n-----END SSH SIGNATURE-----\n")
}

// str is b as an SSH wire string.
func str(b []byte) []byte { return append(binary.BigEndian.AppendUint32(nil, uint32(len(b))), b...) }

// AllowedSigner is the release key as an ssh-keygen allowed_signers line,
// as the install docs publish it.
func AllowedSigner(pub ed25519.PublicKey) string {
	return SumsNamespace + " ssh-ed25519 " + base64.StdEncoding.EncodeToString(slices.Concat(str([]byte("ssh-ed25519")), str(pub)))
}
