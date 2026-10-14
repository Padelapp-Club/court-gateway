package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/term"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/doctor"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/drivers"
	"github.com/Padelapp-Club/court-gateway/internal/service"
	"github.com/Padelapp-Club/court-gateway/internal/setup"
)

// stdin and terminal detection are variables so tests can drive the
// interactive setup.
var (
	stdin io.Reader = os.Stdin
	// installService is what setup --install-service runs after success.
	installService = cmdInstallService
	isTerminal     = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	readSecret     = func() (string, error) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		return string(b), err
	}
)

func cmdSetup(args []string, stdout, stderr io.Writer, getenv func(string) string, defaultConfig string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var in setup.Input
	nonInteractive := fs.Bool("non-interactive", false, "never prompt; take everything from flags and the environment")
	asJSON := fs.Bool("json", false, "print the result as JSON (no secrets)")
	passwordStdin := fs.Bool("password-stdin", false, "read the CRM password from the first line of stdin")
	withService := fs.Bool("install-service", false, "after a successful setup, install and start the service (as install-service does)")
	fs.StringVar(&in.ConfigPath, "config", defaultConfig, "where to write gateway.yaml (.env goes next to it)")
	fs.BoolVar(&in.Force, "force", false, "overwrite existing gateway.yaml and .env")
	// Flags have no environment defaults: the flag package prints defaults
	// in --help and in the error for a mistyped flag, and the join code is a
	// secret. The environment is read after parsing.
	fs.StringVar(&in.HubURL, "hub-url", "", "hub address (env GATEWAY_HUB_URL)")
	fs.StringVar(&in.HubCAFile, "hub-ca-file", "", "trust only this CA certificate for the hub (a self-hosted hub, or pinning)")
	fs.StringVar(&in.HubKeyFingerprint, "hub-key-fingerprint", "", "fingerprint of the hub's key, given with the join code (env GATEWAY_HUB_KEY_FINGERPRINT); a release's own hub needs none")
	fs.StringVar(&in.JoinCode, "join-code", "", "one-time join code (env GATEWAY_JOIN_CODE)")
	fs.StringVar(&in.CRMType, "crm", "viva", "CRM type")
	fs.StringVar(&in.Username, "crm-username", "", "CRM administrator login (env CRM_USERNAME)")
	fs.StringVar(&in.AuthURL, "crm-auth-url", "", "CRM login endpoint override (test doubles)")
	fs.StringVar(&in.APIURL, "crm-api-url", "", "CRM API endpoint override (test doubles)")
	if fs.Parse(args) != nil {
		return 2
	}
	in.HubURL = cmp.Or(in.HubURL, getenv("GATEWAY_HUB_URL"), defaultHubURL)
	in.HubKeyFingerprint = strings.TrimSpace(cmp.Or(in.HubKeyFingerprint, getenv("GATEWAY_HUB_KEY_FINGERPRINT")))
	in.HubKey = embeddedHubKey(in.HubURL)
	in.Username = cmp.Or(in.Username, getenv("CRM_USERNAME"))
	// the CRM password is never a flag (it would land in shell history and
	// in the process list); the join code may be, it is single-use
	in.JoinCode = strings.TrimSpace(cmp.Or(in.JoinCode, getenv("GATEWAY_JOIN_CODE")))
	in.Password = getenv("CRM_PASSWORD")
	reader := bufio.NewReader(stdin)
	if *passwordStdin {
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(stderr, "setup: пароль не прочитан из stdin")
			return 2
		}
		in.Password = strings.TrimRight(line, "\r\n")
	}

	if !*nonInteractive {
		if err := prompt(&in, reader, stderr); err != nil {
			fmt.Fprintf(stderr, "setup: %v\n", err)
			return 2
		}
	}
	if !*asJSON {
		fmt.Fprintln(stderr, "Читаю CRM и подключаюсь к хабу…")
	}
	deps := setup.Deps{Version: version, ProtectSecrets: protectSecrets, Accounts: setupAccounts(getenv)}
	res, err := setup.Run(context.Background(), in, deps)
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	} else {
		printSetup(stdout, res)
	}
	if err != nil {
		return 1
	}
	if *withService {
		out := stdout
		if *asJSON {
			out = stderr // stdout carries the JSON result only
		}
		return installService([]string{"-config", res.Config}, out, stderr, defaultConfig, false)
	}
	return 0
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// cmdSetPassword replaces the CRM password in .env after the CRM
// administrator's password changed; no join code is needed.
func cmdSetPassword(args []string, stdout, stderr io.Writer, getenv func(string) string, defaultConfig string) int {
	fs := flag.NewFlagSet("set-password", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfig, "gateway.yaml (.env next to it is changed)")
	passwordStdin := fs.Bool("password-stdin", false, "read the new CRM password from the first line of stdin")
	if fs.Parse(args) != nil {
		return 2
	}
	password := getenv("CRM_PASSWORD")
	switch {
	case *passwordStdin:
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(stderr, "set-password: пароль не прочитан из stdin")
			return 2
		}
		password = strings.TrimRight(line, "\r\n")
	case password == "" && isTerminal():
		fmt.Fprint(stderr, "Новый пароль администратора CRM (не отображается): ")
		var err error
		password, err = readSecret()
		fmt.Fprintln(stderr)
		if err != nil {
			fmt.Fprintln(stderr, "set-password: ввод прерван")
			return 2
		}
	}
	if err := setup.SetPassword(context.Background(), *cfgPath, password, getenv, setup.Deps{ProtectSecrets: protectSecrets}); err != nil {
		fmt.Fprintf(stderr, "set-password: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, passwordSetHint(fileExists))
	return 0
}

// passwordSetHint is what set-password prints after success. The service
// re-reads .env by itself; a container has the password in its
// environment from its creation, and is created again.
func passwordSetHint(exists func(string) bool) string {
	const recreate = "docker compose up -d --force-recreate court-gateway"
	if exists("/.dockerenv") {
		return "Пароль CRM проверен и записан в .env. Пересоздайте контейнер шлюза: " + recreate + " (пароль попадает в контейнер при создании, перезапуска мало)"
	}
	return "Пароль CRM проверен и записан в .env. Служба шлюза подхватит его сама в течение минуты, перезапуск не нужен (шлюз в Docker: " + recreate + ")"
}

// embeddedHubKey is the hub key a release embeds, for its own hub only.
func embeddedHubKey(hubURL string) string {
	if defaultHubKey == "" || strings.TrimRight(hubURL, "/") != strings.TrimRight(defaultHubURL, "/") {
		return ""
	}
	return defaultHubKey
}

func prompt(in *setup.Input, r *bufio.Reader, w io.Writer) error {
	line := func(q, def string) (string, error) {
		if def != "" {
			fmt.Fprintf(w, "%s [%s]: ", q, def)
		} else {
			fmt.Fprintf(w, "%s: ", q)
		}
		s, err := r.ReadString('\n')
		if err != nil && s == "" {
			return "", fmt.Errorf("ввод прерван")
		}
		s = strings.TrimSpace(s)
		if s == "" {
			s = def
		}
		return s, nil
	}
	// secret returns what was typed as is: spaces may belong to a password
	secret := func(q string) (string, error) {
		fmt.Fprintf(w, "%s (не отображается): ", q)
		if isTerminal() {
			s, err := readSecret()
			fmt.Fprintln(w)
			return s, err
		}
		s, err := r.ReadString('\n')
		if err != nil && s == "" {
			return "", fmt.Errorf("ввод прерван")
		}
		return strings.TrimRight(s, "\r\n"), nil
	}
	var err error
	fmt.Fprintln(w, "Настройка court-gateway. Понадобятся логин и пароль администратора Viva и код подключения.")
	fmt.Fprintln(w, "Клубы, корты и часы работы здесь не задаются: их подключит оператор на хабе.")
	if in.HubURL == "" {
		if in.HubURL, err = line("Адрес хаба (пришёл вместе с кодом подключения)", ""); err != nil {
			return err
		}
		in.HubKey = embeddedHubKey(in.HubURL)
	}
	if in.HubKey == "" && in.HubKeyFingerprint == "" {
		if in.HubKeyFingerprint, err = line("Отпечаток ключа хаба (пришёл вместе с кодом подключения)", ""); err != nil {
			return err
		}
	}
	if in.CRMType == "viva" {
		if in.Username == "" {
			if in.Username, err = line("Логин администратора Viva (e-mail)", ""); err != nil {
				return err
			}
		}
		if in.Password == "" {
			if in.Password, err = secret("Пароль администратора Viva"); err != nil {
				return err
			}
		}
	}
	if in.JoinCode == "" {
		if in.JoinCode, err = secret("Код подключения"); err != nil {
			return err
		}
		in.JoinCode = strings.TrimSpace(in.JoinCode)
	}
	return nil
}

// demoCRMEnv=1 lets setup take the demo CRM (--crm demo): for tests and
// demonstrations, never offered to a club.
const demoCRMEnv = "GATEWAY_DEMO_CRM"

// setupAccounts are the CRM types setup takes.
func setupAccounts(getenv func(string) string) map[string]driver.AccountFactory {
	if getenv(demoCRMEnv) == "1" {
		return drivers.Accounts
	}
	return map[string]driver.AccountFactory{"viva": drivers.Accounts["viva"]}
}

func printSetup(w io.Writer, r setup.Result) {
	if !r.OK {
		fmt.Fprintf(w, "Не получилось (%s): %s\n", r.Error.Code, r.Error.Message)
		return
	}
	fmt.Fprintf(w, "Готово. Установка: %s (%s), хаб: %s\n", r.Installation.Name, r.Installation.ID, r.Hub)
	fmt.Fprintf(w, "Конфигурация: %s\nСекреты:      %s (доступ только владельцу)\n", r.Config, r.EnvFile)
	if len(r.Warnings) > 0 {
		fmt.Fprintln(w, "Проверьте:")
		for _, x := range r.Warnings {
			fmt.Fprintf(w, "  - %s\n", x.Message)
		}
	}
	fmt.Fprintln(w, "Дальше: court-gateway install-service, затем court-gateway doctor.")
	fmt.Fprintln(w, "Больше ничего делать не нужно: клубы подключает оператор на хабе, шлюз получит их сам.")
}

func cmdDoctor(args []string, stdout, stderr io.Writer, getenv func(string) string, defaultConfig string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfig, "config file")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if fs.Parse(args) != nil {
		return 2
	}
	rep := doctor.Run(context.Background(), doctor.Options{
		ConfigPath: *cfgPath, Getenv: getenv, Factories: drivers.Factories, Accounts: drivers.Accounts, Version: version, ReleaseKey: releaseKey,
	})
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else {
		mark := map[doctor.Status]string{doctor.OK: "ок  ", doctor.Warn: "ВНИМ", doctor.Fail: "ОШИБ", doctor.Skip: " -  "}
		fmt.Fprintf(stdout, "court-gateway %s (%s)\n", rep.Version, rep.Build)
		if rep.Installation != "" {
			fmt.Fprintf(stdout, "Установка %s: конфигурация клубов версии %d (%s); служба применила версию %d и обслуживает: %s\n",
				rep.Installation, rep.ConfigVersion, listOrNone(rep.Clubs), rep.AppliedConfigVersion, listOrNone(rep.ClubsServed))
		}
		for _, c := range rep.Checks {
			fmt.Fprintf(stdout, "[%s] %-17s %s\n", mark[c.Status], c.Code, c.Message)
			if c.Hint != "" && c.Status != doctor.OK {
				fmt.Fprintf(stdout, "       → %s\n", c.Hint)
			}
		}
		if rep.OK {
			fmt.Fprintln(stdout, "Всё в порядке.")
		} else {
			fmt.Fprintln(stdout, "Есть ошибки — см. строки ОШИБ.")
		}
	}
	if !rep.OK {
		return 1
	}
	return 0
}

func listOrNone(v []string) string {
	if len(v) == 0 {
		return "клубов нет"
	}
	return strings.Join(v, ", ")
}

func cmdInstallService(args []string, stdout, stderr io.Writer, defaultConfig string, uninstall bool) int {
	name := "install-service"
	if uninstall {
		name = "uninstall-service"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfig, "config file")
	printOnly := fs.Bool("print", false, "only print what would be written and run")
	runAs := fs.String("run-as", "", "Linux/macOS: user that runs the service (default: the user who called sudo)")
	goos := fs.String("os", runtime.GOOS, "target OS for --print (linux, darwin, windows)")
	if fs.Parse(args) != nil {
		return 2
	}
	if !uninstall {
		if _, err := os.Stat(*cfgPath); err != nil {
			fmt.Fprintf(stderr, "%s: нет файла %s — сначала выполните court-gateway setup\n", name, *cfgPath)
			return 2
		}
	}
	// macOS without sudo: a per-user LaunchAgent; uninstall under sudo
	// removes both kinds
	system := runtime.GOOS != "darwin" || os.Geteuid() == 0
	spec, err := service.CurrentSpec(*cfgPath, system)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
	if *runAs != "" {
		spec.User = *runAs
	}
	var plan service.Plan
	if uninstall {
		plan, err = service.UninstallPlan(*goos, spec)
	} else {
		plan, err = service.MakePlan(*goos, spec)
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
	if *printOnly || *goos != runtime.GOOS {
		for path, data := range plan.Files {
			fmt.Fprintf(stdout, "# %s\n%s\n", path, data)
		}
		for _, c := range plan.Commands {
			fmt.Fprintf(stdout, "$ %s\n", strings.Join(c, " "))
		}
		return 0
	}
	write := func(path, data string) error {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(data), 0o644)
	}
	if !uninstall && protectSecrets != nil {
		// the folder and .env are closed and verified before the service
		// that reads them is registered
		for _, p := range []string{filepath.Dir(spec.Config), filepath.Join(filepath.Dir(spec.Config), core.EnvFileName)} {
			if _, err := os.Stat(p); err != nil {
				continue
			}
			if err := protectSecrets(p); err != nil {
				fmt.Fprintf(stderr, "%s: не удалось закрыть %s от других пользователей (нужны права администратора): %v\n", name, p, err)
				return 1
			}
		}
	}
	if err := service.Install(plan, service.ExecRunner, write); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
	if uninstall {
		fmt.Fprintln(stdout, "Служба удалена.")
		return 0
	}
	fmt.Fprintln(stdout, plan.Done)
	for _, n := range plan.Notes {
		fmt.Fprintln(stdout, "  "+n)
	}
	fmt.Fprintln(stdout, "Проверка: court-gateway doctor")
	return 0
}
