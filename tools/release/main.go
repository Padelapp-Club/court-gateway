// Command release prepares signed release metadata. It is a maintainer
// tool and not part of the gateway binary.
//
//	go run ./tools/release keygen -out release.key        (prints the public key; keep the .key file offline)
//	go run ./tools/release manifest -version 0.1.0 -dist dist -key release.key \
//	    -base-url https://github.com/Padelapp-Club/court-gateway/releases/download/v0.1.0
//
// keygen also prints, on stderr, the release key as the allowed_signers
// line the install docs publish; manifest also signs dist/SHA256SUMS.
//
// The private key must never be committed (.gitignore lists *.key).
package main

import (
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/update"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: release keygen|manifest [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "keygen":
		fs := flag.NewFlagSet("keygen", flag.ExitOnError)
		out := fs.String("out", "release.key", "private key file to create")
		_ = fs.Parse(os.Args[2:])
		priv, pub, err := update.GenerateKey()
		check(err)
		f, err := os.OpenFile(*out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		check(err)
		_, err = f.Write(priv)
		check(err)
		check(f.Close())
		fmt.Println(pub)
		key, err := update.ParsePrivateKey(priv)
		check(err)
		fmt.Fprintln(os.Stderr, "for README.md, AGENTS.md and deploy/cloud-init.yaml:", allowedSigner(key.Public().(ed25519.PublicKey)))
	case "manifest":
		fs := flag.NewFlagSet("manifest", flag.ExitOnError)
		ver := fs.String("version", "", "release version")
		dist := fs.String("dist", "dist", "directory with the builds")
		base := fs.String("base-url", "", "download URL prefix of the builds")
		keyFile := fs.String("key", "release.key", "private key file")
		_ = fs.Parse(os.Args[2:])
		data, err := os.ReadFile(*keyFile)
		check(err)
		priv, err := update.ParsePrivateKey(data)
		check(err)
		m, err := update.BuildManifest(*ver, *dist, *base, time.Now())
		check(err)
		check(os.WriteFile(filepath.Join(*dist, "manifest.json"), m, 0o644))
		check(os.WriteFile(filepath.Join(*dist, "manifest.json.sig"), update.Sign(priv, m), 0o644))
		sums, err := os.ReadFile(filepath.Join(*dist, "SHA256SUMS"))
		check(err)
		check(os.WriteFile(filepath.Join(*dist, "SHA256SUMS.sig"), signSums(priv, sums), 0o644))
		fmt.Println("wrote", filepath.Join(*dist, "manifest.json"), "manifest.json.sig and SHA256SUMS.sig")
	default:
		fmt.Fprintln(os.Stderr, "unknown command", os.Args[1])
		os.Exit(2)
	}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

// sumsNamespace is the namespace of the SSH signature of SHA256SUMS.
const sumsNamespace = "court-gateway"

// signSums returns SHA256SUMS.sig: the SSH signature of sums (the format
// of ssh-keygen -Y sign, namespace sumsNamespace). A first install checks
// it with ssh-keygen -Y verify against the release key the install docs
// publish (allowedSigner), before it trusts any program it downloaded.
func signSums(priv ed25519.PrivateKey, sums []byte) []byte {
	hash := sha512.Sum512(sums)
	fields := slices.Concat(str([]byte(sumsNamespace)), str(nil), str([]byte("sha512")))
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

// allowedSigner is the release key as an ssh-keygen allowed_signers line,
// as the install docs publish it.
func allowedSigner(pub ed25519.PublicKey) string {
	return sumsNamespace + " ssh-ed25519 " + base64.StdEncoding.EncodeToString(slices.Concat(str([]byte("ssh-ed25519")), str(pub)))
}
