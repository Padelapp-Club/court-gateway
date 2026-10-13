// Command release prepares signed release metadata. It is a maintainer
// tool and not part of the gateway binary.
//
//	go run ./tools/release keygen -out release.key        (prints the public key; keep the .key file offline)
//	go run ./tools/release manifest -version 0.1.0 -dist dist -key release.key \
//	    -base-url https://github.com/Padelapp-Club/court-gateway/releases/download/v0.1.0
//
// The private key must never be committed (.gitignore lists *.key).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
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
		fmt.Println("wrote", filepath.Join(*dist, "manifest.json"), "and manifest.json.sig")
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
