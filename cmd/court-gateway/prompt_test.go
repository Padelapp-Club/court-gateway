package main

import (
	"bufio"
	"io"
	"strings"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/setup"
)

// A password typed at the terminal is kept byte for byte, spaces at the
// ends included, as the environment and --password-stdin keep it; the
// join code is trimmed.
func TestTerminalPasswordIsNotTrimmed(t *testing.T) {
	oldTerm, oldSecret := isTerminal, readSecret
	defer func() { isTerminal, readSecret = oldTerm, oldSecret }()
	answers := []string{" synthetic password ", " AAAA-AAAA-AAAA-AAAA "}
	isTerminal = func() bool { return true }
	readSecret = func() (string, error) { a := answers[0]; answers = answers[1:]; return a, nil }
	in := setup.Input{HubURL: "https://hub.example.test", CRMType: "viva", CRM: map[string]string{"username": "user@example.test"}, HubKeyFingerprint: "AAAA"}
	if err := prompt(&in, bufio.NewReader(strings.NewReader("")), io.Discard); err != nil {
		t.Fatal(err)
	}
	if in.Password != " synthetic password " || in.JoinCode != "AAAA-AAAA-AAAA-AAAA" {
		t.Fatalf("password %q, join code %q", in.Password, in.JoinCode)
	}
}
