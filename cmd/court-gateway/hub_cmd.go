package main

import (
	"context"
	"errors"
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
	if len(args) >= 1 && args[0] == "key" {
		return cmdHubKey(args[1:], stdout, stderr, getenv, defaultConfig)
	}
	if len(args) >= 1 && args[0] == "keygen" {
		return cmdHubKeygen(args[1:], stdout, stderr)
	}
	if len(args) > 0 && (len(args[0]) == 0 || args[0][0] != '-') {
		fmt.Fprintf(stderr, "hub: неизвестная команда %q (есть: hub, hub join-code create, hub gateway revoke, hub key, hub keygen)\n", args[0])
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
		return hubFail(stderr, err)
	}
	log := core.NewLogger(stderr, slog.LevelInfo, s.Secrets)
	store, err := hub.Open(s, nil)
	if err != nil {
		return hubFail(stderr, err)
	}
	defer store.Close()
	srv, err := hub.NewServer(s, store, hub.Options{Logger: log, Version: version})
	if err != nil {
		return hubFail(stderr, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := hub.Run(ctx, s, srv, log); err != nil {
		log.Error("hub stopped", "error", err)
		return 1
	}
	return 0
}

func hubFail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "court-gateway hub: %v\n", err)
	return 2
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
	inst := fs.String("installation", "", "installation id (one CRM account): lower-case latin letters, digits, dashes")
	name := fs.String("name", "", "installation name, used if the code creates the installation")
	ttl := fs.Duration("ttl", hub.DefaultJoinCodeTTL, "how long the code stays valid")
	if fs.Parse(args) != nil {
		return 2
	}
	s, err := loadHub(*cfgPath, getenv)
	if err != nil {
		return hubFail(stderr, err)
	}
	key, err := hub.PublicKey(s)
	if err != nil {
		return hubFail(stderr, err)
	}
	st, err := hub.OpenState(s)
	if err != nil {
		return hubFail(stderr, err)
	}
	defer st.Close()
	jc, err := hub.CreateJoinCode(st, *inst, *name, *ttl, time.Now())
	switch {
	case errors.Is(err, hub.ErrInvalidID):
		fmt.Fprintf(stderr, "court-gateway hub: --installation: «%s» — от 2 до 63 символов: строчные латинские буквы, цифры и дефис\n", *inst)
		return 2
	case errors.Is(err, hub.ErrInvalidName):
		fmt.Fprintln(stderr, "court-gateway hub: --name: не длиннее 200 символов, без управляющих символов")
		return 2
	case errors.Is(err, hub.ErrInvalidTTL):
		fmt.Fprintf(stderr, "court-gateway hub: --ttl: больше нуля и не дольше %s\n", hub.MaxJoinCodeTTL)
		return 2
	case err != nil:
		return hubFail(stderr, err)
	}
	fp := core.HubKeyFingerprint(key)
	fmt.Fprintf(stdout, "Код подключения установки %s: %s\n", jc.Installation, jc.Code)
	fmt.Fprintf(stdout, "Действует до %s UTC, один раз.\n", jc.Expires.UTC().Format("2006-01-02 15:04"))
	fmt.Fprintf(stdout, "Отпечаток ключа хаба: %s\n", fp)
	if s.PublicURL != "" {
		fmt.Fprintf(stdout, "Адрес хаба: %s\n", s.PublicURL)
		fmt.Fprintf(stdout, "Команда для клуба: court-gateway setup --hub-url %s --hub-key-fingerprint %s\n", s.PublicURL, fp)
	}
	return 0
}

// cmdHubKey prints the hub's public signing key (DEFAULT_HUB_KEY of a
// release build) and its fingerprint.
func cmdHubKey(args []string, stdout, stderr io.Writer, getenv func(string) string, defaultConfig string) int {
	fs := flag.NewFlagSet("hub key", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfig, "hub config file")
	if fs.Parse(args) != nil {
		return 2
	}
	s, err := loadHub(*cfgPath, getenv)
	if err == nil {
		var key []byte
		if key, err = hub.PublicKey(s); err == nil {
			fmt.Fprintf(stdout, "key: %s\nfingerprint: %s\n", core.EncodeHubKey(key), core.HubKeyFingerprint(key))
			return 0
		}
	}
	return hubFail(stderr, err)
}

// cmdHubKeygen makes the hub's signing key in a new file, once, for
// signing_key_file: the key never goes into the database.
func cmdHubKeygen(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hub keygen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "new key file (must not exist), for signing_key_file in hub.yaml")
	if fs.Parse(args) != nil {
		return 2
	}
	if *out == "" {
		fmt.Fprintln(stderr, "court-gateway hub keygen: укажите -out <файл> — файл ключа подписи хаба для signing_key_file")
		return 2
	}
	key, err := hub.WriteSigningKey(*out)
	if err != nil {
		return hubFail(stderr, err)
	}
	fmt.Fprintf(stdout, "key: %s\nfingerprint: %s\n", core.EncodeHubKey(key), core.HubKeyFingerprint(key))
	fmt.Fprintf(stderr, "Ключ подписи хаба записан в %s (другим пользователям закрыт; на Windows — доступ только администраторам и системе). Храните копию: с другим ключом уже подключённые шлюзы хаб не примут.\n", *out)
	return 0
}

func cmdRevoke(args []string, stdout, stderr io.Writer, getenv func(string) string, defaultConfig string) int {
	fs := flag.NewFlagSet("hub gateway revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfig, "hub config file")
	inst := fs.String("installation", "", "installation id")
	if fs.Parse(args) != nil {
		return 2
	}
	s, err := loadHub(*cfgPath, getenv)
	if err != nil {
		return hubFail(stderr, err)
	}
	st, err := hub.OpenState(s)
	if err != nil {
		return hubFail(stderr, err)
	}
	defer st.Close()
	if err := hub.RevokeGateway(st, *inst); err != nil {
		if errors.Is(err, hub.ErrNotRegistered) {
			fmt.Fprintf(stderr, "court-gateway hub: у установки %s нет зарегистрированного шлюза\n", *inst)
		} else {
			fmt.Fprintf(stderr, "court-gateway hub: %v\n", err)
		}
		return 1
	}
	fmt.Fprintf(stdout, "Подключение установки %s отозвано; шлюз потеряет доступ при следующем запросе. Клубы установки остались на хабе.\n", *inst)
	return 0
}
