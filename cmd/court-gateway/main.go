// Command court-gateway serves a CRM-agnostic partner API for one club.
//
//	court-gateway serve       [-config gateway.yaml]
//	court-gateway healthcheck [-config gateway.yaml] [-addr host:port]
//	court-gateway version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // the binary must know time zones on hosts without a zoneinfo database

	"gopkg.in/yaml.v3"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/demo"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// factories lists the available CRM drivers by crm.type.
var factories = map[string]driver.Factory{
	"viva": viva.New,
	"demo": demo.New,
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

const usage = `court-gateway — партнёрский API клуба поверх его CRM

Команды:
  serve        запустить сервис
  healthcheck  проверить готовность запущенного сервиса (код выхода 0 — готов)
  setup        собрать черновик конфигурации (пока не реализовано)
  version      показать версию

Параметры serve и healthcheck:
  -config путь   файл конфигурации (по умолчанию gateway.yaml или $GATEWAY_CONFIG)
`

func run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	defaultConfig := getenv("GATEWAY_CONFIG")
	if defaultConfig == "" {
		defaultConfig = "gateway.yaml"
	}
	switch args[0] {
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		fs.SetOutput(stderr)
		cfgPath := fs.String("config", defaultConfig, "config file")
		if fs.Parse(args[1:]) != nil {
			return 2
		}
		return serve(*cfgPath, stderr, getenv)
	case "healthcheck":
		fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
		fs.SetOutput(stderr)
		cfgPath := fs.String("config", defaultConfig, "config file")
		addr := fs.String("addr", "", "host:port of the running service (overrides listen from the config)")
		if fs.Parse(args[1:]) != nil {
			return 2
		}
		return healthcheck(*cfgPath, *addr, stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, "court-gateway", version)
		return 0
	case "setup":
		fmt.Fprintln(stdout, "not yet: команда setup появится в следующей версии")
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "неизвестная команда %q\n\n%s", args[0], usage)
		return 2
	}
}

func serve(cfgPath string, stderr io.Writer, getenv func(string) string) int {
	s, err := core.LoadFile(cfgPath, getenv)
	if err != nil {
		fmt.Fprintf(stderr, "court-gateway: %v\n", err)
		return 2
	}
	log := core.NewLogger(stderr, slog.LevelInfo, s.Secrets)
	s.Driver.Logger = log
	drv, err := core.BuildDriver(s, factories)
	if err != nil {
		fmt.Fprintf(stderr, "court-gateway: %v\n", err)
		return 2
	}
	srv, err := core.NewServer(s, drv, log)
	if err != nil {
		fmt.Fprintf(stderr, "court-gateway: %v\n", err)
		return 2
	}
	httpSrv := &http.Server{
		Addr:              s.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	log.Info("started", "version", version, "listen", s.Listen, "crm", s.CRMType, "courts", len(s.Courts))
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("server stopped", "error", err)
			return 1
		}
	case <-ctx.Done():
		log.Info("shutting down")
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shCtx); err != nil {
			log.Error("shutdown", "error", err)
			return 1
		}
	}
	return 0
}

// healthcheck asks the running service for /health/ready. It reads only
// `listen` from the config, so it needs no secrets.
func healthcheck(cfgPath, addr string, stdout, stderr io.Writer) int {
	if addr == "" {
		listen := core.DefaultListen
		if data, err := os.ReadFile(cfgPath); err == nil {
			var c struct {
				Listen string `yaml:"listen"`
			}
			if yaml.Unmarshal(data, &c) == nil && c.Listen != "" {
				listen = c.Listen
			}
		}
		addr = listen
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintf(stderr, "healthcheck: неверный адрес %q\n", addr)
		return 1
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: core.ReadyTimeout + 2*time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/health/ready")
	if err != nil {
		fmt.Fprintln(stderr, "healthcheck: сервис не отвечает")
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	fmt.Fprintf(stdout, "%d %s", resp.StatusCode, body)
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
