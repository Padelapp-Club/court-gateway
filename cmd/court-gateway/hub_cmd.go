package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
)

func cmdHub(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	defaultConfig := getenv("HUB_CONFIG")
	if defaultConfig == "" {
		defaultConfig = "hub.yaml"
	}
	if len(args) >= 2 && args[0] == "join-code" && args[1] == "create" {
		return cmdJoinCode(args[2:], stdout, stderr, getenv, defaultConfig)
	}
	if len(args) >= 2 && args[0] == "gateway" && args[1] == "revoke" {
		return cmdRevoke(args[2:], stdout, stderr, getenv, defaultConfig)
	}
	if len(args) > 0 && (len(args[0]) == 0 || args[0][0] != '-') {
		fmt.Fprintf(stderr, "hub: неизвестная команда %q (есть: hub, hub join-code create, hub gateway revoke)\n", args[0])
		return 2
	}
	fs := flag.NewFlagSet("hub", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfig, "hub config file")
	if fs.Parse(args) != nil {
		return 2
	}
	s, err := loadHub(*cfgPath, getenv)
	if err != nil {
		fmt.Fprintf(stderr, "court-gateway hub: %v\n", err)
		return 2
	}
	log := core.NewLogger(stderr, slog.LevelInfo, s.Secrets)
	store, err := hub.OpenStore(s.StateDir, nil)
	if err != nil {
		fmt.Fprintf(stderr, "court-gateway hub: %v\n", err)
		return 2
	}
	srv, err := hub.NewServer(s, store, hub.Options{Logger: log, Version: version})
	if err != nil {
		fmt.Fprintf(stderr, "court-gateway hub: %v\n", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := hub.Run(ctx, s, srv, log); err != nil {
		log.Error("hub stopped", "error", err)
		return 1
	}
	return 0
}

func loadHub(cfgPath string, getenv func(string) string) (*hub.Settings, error) {
	getenv, err := core.EnvWithDotEnv(cfgPath, getenv)
	if err != nil {
		return nil, err
	}
	return hub.LoadFile(cfgPath, getenv)
}

func cmdJoinCode(args []string, stdout, stderr io.Writer, getenv func(string) string, defaultConfig string) int {
	fs := flag.NewFlagSet("hub join-code create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfig, "hub config file")
	club := fs.String("club", "", "club id: lower-case latin letters, digits, dashes")
	name := fs.String("name", "", "club name as people know it")
	ttl := fs.Duration("ttl", hub.DefaultJoinCodeTTL, "how long the code stays valid")
	if fs.Parse(args) != nil {
		return 2
	}
	s, err := loadHub(*cfgPath, getenv)
	if err != nil {
		fmt.Fprintf(stderr, "court-gateway hub: %v\n", err)
		return 2
	}
	code, expires, err := hub.CreateJoinCode(s.StateDir, *club, *name, *ttl, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "court-gateway hub: %v\n", err)
		return 2
	}
	fmt.Fprintf(stdout, "Код подключения клуба %s: %s\n", *club, code)
	fmt.Fprintf(stdout, "Действует до %s UTC, один раз.\n", expires.UTC().Format("2006-01-02 15:04"))
	if s.PublicURL != "" {
		fmt.Fprintf(stdout, "Адрес хаба: %s\n", s.PublicURL)
		fmt.Fprintf(stdout, "Команда для клуба: court-gateway setup --hub-url %s\n", s.PublicURL)
	}
	return 0
}

func cmdRevoke(args []string, stdout, stderr io.Writer, getenv func(string) string, defaultConfig string) int {
	fs := flag.NewFlagSet("hub gateway revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfig, "hub config file")
	club := fs.String("club", "", "club id")
	if fs.Parse(args) != nil {
		return 2
	}
	s, err := loadHub(*cfgPath, getenv)
	if err != nil {
		fmt.Fprintf(stderr, "court-gateway hub: %v\n", err)
		return 2
	}
	if err := hub.RevokeGateway(s.StateDir, *club); err != nil {
		fmt.Fprintf(stderr, "court-gateway hub: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Подключение клуба %s отозвано; шлюз потеряет доступ при следующем запросе.\n", *club)
	return 0
}
