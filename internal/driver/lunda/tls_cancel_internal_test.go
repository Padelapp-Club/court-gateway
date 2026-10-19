package lunda

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"
)

// A stop that lands just as the wss TLS handshake returns
// closes the raw connection; it never reads the connection variable the
// TLS wrapper replaces (a data race on an interface value: run with -race).
func TestTLSHandshakeCancelHasNoRace(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "lunda.test"}, DNSNames: []string{"lunda.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	oldDial, oldRoots := wsDialer, wsRoots
	t.Cleanup(func() { wsDialer, wsRoots = oldDial, oldRoots })
	wsRoots = x509.NewCertPool()
	wsRoots.AddCert(cert)
	config := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS13}
	for i := 0; i < 300; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		a, b := net.Pipe()
		wsDialer = func(context.Context, string, string) (net.Conn, error) { return a, nil }
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer b.Close()
			server := tls.Server(b, config)
			_ = server.SetDeadline(time.Now().Add(time.Second))
			_ = server.Handshake()
			cancel() // the stop lands as the client's handshake returns
		}()
		_, _ = dialWS(ctx, "wss://lunda.test/api-ws/websocket-listen", "ticket")
		cancel()
		<-done
	}
}
