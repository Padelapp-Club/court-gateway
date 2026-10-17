package lunda

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"strings"
)

// Lunda never sends a token in clear: every login, club switch and renewal
// sends a fresh RSA public key, and the token comes back encrypted with it
// (RSA-OAEP, SHA-256, no label — WebCrypto's RSA-OAEP), cut into chunks
// that are encrypted and base64-encoded one by one. The private key lives
// in memory for the one exchange only.

// keyBits is the size of the exchange's key, as the admin client's.
const keyBits = 2048

// maxChunks bounds the chunks of one token (a 336-character token is 2 or
// 3 of them).
const maxChunks = 32

// maxToken bounds a token's length.
const maxToken = 8192

type keyPair struct {
	priv   *rsa.PrivateKey
	public string // SPKI, standard base64: the publicKey parameter
}

func newKeyPair() (*keyPair, error) {
	priv, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, err
	}
	return &keyPair{priv: priv, public: base64.StdEncoding.EncodeToString(der)}, nil
}

// decrypt joins the token from its encrypted chunks. The error never holds
// a chunk or a part of the token.
func (k *keyPair) decrypt(chunks []string) (string, error) {
	if len(chunks) == 0 || len(chunks) > maxChunks {
		return "", errors.New("the encrypted token has no chunks or too many")
	}
	var b strings.Builder
	for _, c := range chunks {
		ct, err := base64.StdEncoding.DecodeString(strings.TrimSpace(c))
		if err != nil {
			return "", errors.New("a chunk of the encrypted token is not base64")
		}
		pt, err := rsa.DecryptOAEP(sha256.New(), nil, k.priv, ct, nil)
		if err != nil {
			return "", errors.New("a chunk of the encrypted token does not decrypt with the exchange's key")
		}
		b.Write(pt)
	}
	tok := b.String()
	if !validToken(tok) {
		return "", errors.New("the decrypted token is not a header-safe string")
	}
	return tok, nil
}

// validToken: a token goes into a header as is, so it is printable ASCII
// without spaces, and not absurdly long.
func validToken(s string) bool {
	if s == "" || len(s) > maxToken {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
