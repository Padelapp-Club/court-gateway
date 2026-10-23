package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda/lundastub"
)

// setup with the second CRM, as the person runs it: by the flag (refused
// without a person, then done in the terminal) or by answering yes; the
// config written takes the mirror keys of the flags and loads, and the
// Lunda session it saved serves the second CRM's driver.
func TestSetupMirrorThroughTheCLI(t *testing.T) {
	for _, mode := range []string{"flag", "yes"} {
		t.Run(mode, func(t *testing.T) {
			s := newStack(t)
			stub := lundastub.New(lundastub.Club{UID: "review-club", Name: "Review"})
			srv := httptest.NewServer(stub)
			defer srv.Close()
			code := s.hub.JoinCode("review-club", "Review")
			get := env{"CRM_PASSWORD": crmPass, "GATEWAY_JOIN_CODE": code}.get
			args := s.setupArgs("--crm", "viva", "--mirror-api-url", srv.URL+"/api", "--mirror-service-client", "review-client")
			if mode == "flag" {
				args = append(args, "--mirror", "lunda")
			}
			var out, errb bytes.Buffer
			if mode == "flag" {
				rc := run(args, &out, &errb, get)
				r := decodeSetup(t, out.Bytes())
				if rc != 1 || r.Error == nil || r.Error.Code != "LOGIN_REQUIRED" {
					t.Fatalf("noninteractive: rc=%d %s %s", rc, out.String(), errb.String())
				}
				out.Reset()
				errb.Reset()
			}
			args = slices.DeleteFunc(args, func(s string) bool { return s == "--non-interactive" })
			answers := "2\n+79990000000\n246810\n"
			if mode == "yes" {
				answers = "да\n" + answers
			}
			old := stdin
			stdin = strings.NewReader(answers)
			defer func() { stdin = old }()
			rc := run(args, &out, &errb, get)
			r := decodeSetup(t, out.Bytes())
			if rc != 0 || !r.OK || r.Mirror != "lunda" {
				t.Fatalf("interactive: rc=%d %s %s", rc, out.String(), errb.String())
			}
			e, err := core.EnvWithDotEnv(r.Config, get)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := core.LoadFile(r.Config, e)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Mirror == nil || cfg.Mirror.Driver.CRM["service_client"] != "review-client" {
				t.Fatalf("mirror fields not passed: %+v", cfg.Mirror)
			}
			a, err := lunda.NewAccount(cfg.Mirror.Driver)
			if err != nil {
				t.Fatal(err)
			}
			defer driver.EndSession(context.Background(), a)
			if err := a.CheckLogin(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// An installation whose CRM is Lunda is not asked about a second Lunda.
func TestSetupDoesNotOfferLundaToLunda(t *testing.T) {
	s := newStack(t)
	old := stdin
	stdin = strings.NewReader("")
	defer func() { stdin = old }()
	args := slices.DeleteFunc(s.setupArgs("--crm", "lunda"), func(s string) bool { return s == "--non-interactive" })
	var out, errb bytes.Buffer
	run(args, &out, &errb, env{}.get)
	if strings.Contains(errb.String(), "Есть ли у клубов ещё") {
		t.Fatalf("asked: %s", errb.String())
	}
}
