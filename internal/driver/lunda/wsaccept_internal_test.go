package lunda

import (
	"crypto/sha1"
	"encoding/base64"
	"testing"
)

// The key/accept pair of RFC 6455 §1.3, independent of the stub (which a
// wrong GUID in both would pass): found live at Kosmos, 2026-10-17, where
// every upgrade was refused as "not an upgrade to this key".
func TestWebSocketAcceptIsRFC6455(t *testing.T) {
	sum := sha1.Sum([]byte("dGhlIHNhbXBsZSBub25jZQ==" + wsGUID))
	if got, want := base64.StdEncoding.EncodeToString(sum[:]), "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="; got != want {
		t.Fatalf("Sec-WebSocket-Accept = %s, want %s (RFC 6455 §1.3)", got, want)
	}
}
