package main

import (
	"crypto/ed25519"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// SHA256SUMS.sig is what ssh-keygen -Y verify accepts for the release key
// the install docs publish, and only for the SHA256SUMS signed.
func TestSumsSignatureVerifiesWithSSHKeygen(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen")
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	dir := t.TempDir()
	write := func(name, data string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sums := "0123  court-gateway-9.9.9-linux-amd64\n"
	write("allowed_signers", allowedSigner(pub)+"\n")
	write("SHA256SUMS.sig", string(signSums(priv, []byte(sums))))
	for _, c := range []struct {
		sums string
		ok   bool
	}{{sums, true}, {"4567  court-gateway-9.9.9-linux-amd64\n", false}} {
		write("SHA256SUMS", c.sums)
		cmd := exec.Command(keygen, "-Y", "verify", "-f", "allowed_signers", "-I", sumsNamespace, "-n", sumsNamespace, "-s", "SHA256SUMS.sig")
		cmd.Dir = dir
		in, _ := os.Open(filepath.Join(dir, "SHA256SUMS"))
		cmd.Stdin = in
		out, err := cmd.CombinedOutput()
		in.Close()
		if (err == nil) != c.ok {
			t.Fatalf("sums %q: verify %v, want ok=%v\n%s", c.sums, err, c.ok, out)
		}
	}
}
