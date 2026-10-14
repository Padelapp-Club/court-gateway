// Command court-gateway serves a CRM-agnostic partner API for one club and,
// in hub mode, the public hub that partners call and gateways connect to.
// The commands are listed in usage.
package main

import (
	"fmt"
	"io"
	"os"
	"time"
	_ "time/tzdata" // the binary must know time zones on hosts without a zoneinfo database

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
	"github.com/Padelapp-Club/court-gateway/internal/update"
)

// Set at build time with -ldflags "-X main.version=... -X main.releaseKey=...
// -X main.defaultHubURL=... -X main.defaultHubKey=..." (scripts/build.sh).
var (
	version = "dev"
	// releaseKey is the base64 ed25519 public key of release manifests.
	// Empty means a self-built binary: it never updates itself.
	releaseKey = ""
	// defaultHubURL is the hub setup connects to when --hub-url is not
	// given. Releases embed it; a self-built binary needs --hub-url.
	defaultHubURL = ""
	// defaultHubKey is the public signing key (base64) of that hub: setup
	// refuses a registration answer with another key. Without it setup
	// needs the key's fingerprint from the operator.
	defaultHubKey = ""
)

// exitRestart asks the service manager to start the binary again (used
// where a process cannot replace itself, i.e. Windows, after an update).
const exitRestart = 3

func build() string {
	if releaseKey == "" {
		return tunnel.BuildSelf
	}
	return tunnel.BuildRelease
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

const usage = `court-gateway — партнёрский API клуба поверх его CRM

Установка и работа шлюза:
  setup            собрать gateway.yaml и .env: вход в CRM и код подключения к хабу (клубы задаёт хаб)
  set-password     сменить пароль CRM в .env, если его поменяли в CRM (код подключения не нужен)
  serve            запустить шлюз (сам подключается к хабу; входящих портов не открывает)
  doctor           проверить всё: конфигурацию, CRM, связь с хабом, версию (--json — для программ)
  install-service  зарегистрировать службу (systemd, launchd, Windows), чтобы шлюз запускался сам
  uninstall-service  удалить службу
  healthcheck      проверка для Docker: код 0 — готов, 1 — не готов, 2 — служба не запущена или зависла
                   (--live: 0 — служба работает, готова она или нет)
  version          показать версию

Хаб (для оператора):
  hub                        запустить хаб (-config hub.yaml)
  hub join-code create       выдать одноразовый код подключения установки (--installation id --name "Название")
  hub gateway revoke         отозвать подключение установки (--installation id)
  hub key                    показать открытый ключ хаба и его отпечаток

Общий параметр:
  -config путь     gateway.yaml (по умолчанию gateway.yaml или $GATEWAY_CONFIG)
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
		return cmdServe(args[1:], stderr, getenv, defaultConfig)
	case "service":
		return cmdService(args[1:], stderr, getenv, defaultConfig)
	case "healthcheck":
		return cmdHealthcheck(args[1:], stdout, stderr, getenv, defaultConfig)
	case "selfcheck":
		return cmdSelfcheck(args[1:], stdout, stderr, getenv, defaultConfig)
	case "doctor":
		return cmdDoctor(args[1:], stdout, stderr, getenv, defaultConfig)
	case "setup":
		return cmdSetup(args[1:], stdout, stderr, getenv, defaultConfig)
	case "set-password":
		return cmdSetPassword(args[1:], stdout, stderr, getenv, defaultConfig)
	case "install-service":
		return cmdInstallService(args[1:], stdout, stderr, defaultConfig, false)
	case "uninstall-service":
		return cmdInstallService(args[1:], stdout, stderr, defaultConfig, true)
	case "hub":
		return cmdHub(args[1:], stdout, stderr, getenv)
	case "version":
		fmt.Fprintf(stdout, "court-gateway %s (%s, %s)\n", version, build(), update.Platform())
		if running := update.RunningVersion(core.StateDir(defaultConfig, getenv), version, time.Now()); running != version {
			fmt.Fprintf(stdout, "служба работает на версии %s (этот файл — установленный запускающий)\n", running)
		}
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "неизвестная команда %q\n\n%s", args[0], usage)
		return 2
	}
}
