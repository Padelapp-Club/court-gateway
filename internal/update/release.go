package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
		m.Assets[p] = Asset{URL: strings.TrimRight(baseURL, "/") + "/" + name, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
	}
	return json.MarshalIndent(m, "", "  ")
}
