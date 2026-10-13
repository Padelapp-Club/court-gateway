package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
)

// A hub published under a path prefix prints its full public address,
// prefix included and without a trailing slash, for the club's setup.
func TestHubJoinCodePrintsPublicURLWithPath(t *testing.T) {
	dir := t.TempDir()
	hubCfg := filepath.Join(dir, "hub.yaml")
	cfg := "state_dir: " + filepath.Join(dir, "state") + "\npublic_url: https://hub.example.test/gateway-hub/\n"
	if err := os.WriteFile(hubCfg, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if rc := run([]string{"hub", "join-code", "create", "-config", hubCfg, "--installation", "club-x", "--name", "Клуб X"}, &out, io.Discard, env{}.get); rc != 0 {
		t.Fatalf("rc %d", rc)
	}
	printed := out.String()
	for _, want := range []string{
		"Адрес хаба: https://hub.example.test/gateway-hub\n",
		"--hub-url https://hub.example.test/gateway-hub\n",
	} {
		if !strings.Contains(printed, want) {
			t.Fatalf("missing %q in:\n%s", want, printed)
		}
	}
}

// A hub without a tls section listens on plain HTTP, and the container
// healthcheck for hub mode probes it over plain HTTP.
func TestHubPlainHTTPHealthcheck(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	hubCfg := filepath.Join(dir, "hub.yaml")
	cfg := "listen: " + addr + "\npublic_url: https://hub.example.test/gateway-hub\nstate_dir: " + filepath.Join(dir, "state") + "\n"
	if err := os.WriteFile(hubCfg, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	// nothing is listening yet: the probe fails
	if rc := run([]string{"healthcheck", "-hub", hubCfg}, io.Discard, io.Discard, env{}.get); rc != 1 {
		t.Fatalf("healthcheck without a hub: rc %d", rc)
	}
	s, err := loadHub(hubCfg, env{}.get)
	if err != nil {
		t.Fatal(err)
	}
	if s.TLSCertFile != "" {
		t.Fatalf("tls configured: %+v", s)
	}
	store, err := hub.OpenStore(s.StateDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	log := core.NewLogger(io.Discard, slog.LevelInfo, s.Secrets)
	srv, err := hub.NewServer(s, store, hub.Options{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- hub.Run(ctx, s, srv, log) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("hub.Run: %v", err)
		}
	}()
	var out bytes.Buffer
	testkit.WaitFor(t, "hub healthcheck over plain HTTP", 5*time.Second, func() bool {
		out.Reset()
		return run([]string{"healthcheck", "-hub", hubCfg}, &out, io.Discard, env{}.get) == 0
	})
	if !strings.HasPrefix(out.String(), "200 ") {
		t.Fatalf("healthcheck output: %q", out.String())
	}
}
