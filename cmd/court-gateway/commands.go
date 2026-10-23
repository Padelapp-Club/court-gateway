package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
	offered := setupTypes(getenv)
	fs.StringVar(&in.CRMType, "crm", "", "CRM type: "+strings.Join(offered, ", ")+" (default "+drivers.DefaultCRM+")")
	fields := crmFlags(fs)
	mirrorType := fs.String("mirror", "", "second CRM of the same clubs: "+strings.Join(core.MirrorTypes, ", ")+" (default: none; setup asks)")
	mirrorFields := mirrorFlags(fs)
	if fs.Parse(args) != nil {
		return 2
	}
	crmGiven := in.CRMType != ""
	in.CRMType = cmp.Or(in.CRMType, drivers.DefaultCRM)
	in.HubURL = cmp.Or(in.HubURL, getenv("GATEWAY_HUB_URL"), defaultHubURL)
	in.HubKeyFingerprint = strings.TrimSpace(cmp.Or(in.HubKeyFingerprint, getenv("GATEWAY_HUB_KEY_FINGERPRINT")))
	in.HubKey = embeddedHubKey(in.HubURL)
	// the CRM password is never a flag (it would land in shell history and
	// in the process list); the join code may be, it is single-use
	in.JoinCode = strings.TrimSpace(cmp.Or(in.JoinCode, getenv("GATEWAY_JOIN_CODE")))
	in.Password = getenv(core.PasswordEnv)
	reader := bufio.NewReader(stdin)
	if *passwordStdin {
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(stderr, "setup: пароль не прочитан из stdin")
			return 2
		}
		in.Password = strings.TrimRight(line, "\r\n")
	}

	if !*nonInteractive && !crmGiven && len(offered) > 1 {
		t, err := askCRM(reader, stderr, offered)
		if err != nil {
			fmt.Fprintf(stderr, "setup: %v\n", err)
			return 2
		}
		in.CRMType = t
	}
	in.Mirror = strings.ToLower(strings.TrimSpace(*mirrorType))
	if !*nonInteractive && in.Mirror == "" && !slices.Contains(core.MirrorTypes, in.CRMType) {
		m, err := askMirror(reader, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "setup: %v\n", err)
			return 2
		}
		in.Mirror = m
	}
	fillCRM(&in, fields, getenv)
	fillMirror(&in, mirrorFields)
	if !*nonInteractive {
		if err := prompt(&in, reader, stderr); err != nil {
			fmt.Fprintf(stderr, "setup: %v\n", err)
			return 2
		}
	}
	if !*asJSON {
		fmt.Fprintln(stderr, "Читаю CRM и подключаюсь к хабу…")
	}
	deps := setup.Deps{Version: version, ProtectSecrets: protectSecrets, Accounts: setupAccounts(getenv), Note: noteTo(stderr)}
	if !*nonInteractive {
		// a CRM a person logs in to: the login and the service client are
		// asked in this terminal
		stateDir := core.StateDir(cmp.Or(in.ConfigPath, "gateway.yaml"), getenv)
		deps.Login = func(ctx context.Context, il driver.InteractiveLogin) error {
			return loginInteractively(ctx, il, reader, stderr, "", "", stateDir)
		}
		deps.PickClient = pickClient(reader, stderr)
	}
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

// noteTo prints setup's remarks to w (stderr: stdout may carry JSON).
func noteTo(w io.Writer) func(string) {
	return func(s string) { fmt.Fprintln(w, "Примечание: "+s) }
}

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
	if err := setup.SetPassword(context.Background(), *cfgPath, password, getenv, setup.Deps{ProtectSecrets: protectSecrets, Note: noteTo(stderr)}); err != nil {
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
	spec := in.Spec()
	fmt.Fprintf(w, "Настройка court-gateway. Понадобятся %s и код подключения.\n", cmp.Or(spec.Needs, "данные для входа в CRM"))
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
	if in.Mirror != "" {
		mspec, _ := driver.SetupFor(in.Mirror)
		fmt.Fprintf(w, "Для второй CRM (%s) понадобится %s.\n", cmp.Or(mspec.Name, in.Mirror), cmp.Or(mspec.Needs, "вход в неё"))
	}
	for _, f := range spec.Fields {
		if f.Prompt == "" || in.CRM[f.Key] != "" || f.WithMirror && in.Mirror == "" {
			continue
		}
		if in.CRM == nil {
			in.CRM = map[string]string{}
		}
		if in.CRM[f.Key], err = line(f.Prompt, ""); err != nil {
			return err
		}
	}
	for _, x := range spec.Secrets {
		if x.Prompt == "" || in.Secret(x.Env) != "" {
			continue
		}
		v, err := secret(x.Prompt)
		if err != nil {
			return err
		}
		if x.Env == core.PasswordEnv {
			in.Password = v
		} else {
			if in.Secrets == nil {
				in.Secrets = map[string]string{}
			}
			in.Secrets[x.Env] = v
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

// setupAccounts are the CRM types setup takes: every registered one but
// the hidden (demo) ones.
func setupAccounts(getenv func(string) string) map[string]driver.AccountFactory {
	if getenv(demoCRMEnv) == "1" {
		return drivers.Accounts
	}
	out := map[string]driver.AccountFactory{}
	for t, f := range drivers.Accounts {
		if spec, ok := driver.SetupFor(t); ok && !spec.Hidden {
			out[t] = f
		}
	}
	return out
}

// setupTypes are the CRM types setup offers, the default first.
func setupTypes(getenv func(string) string) []string {
	var out []string
	for t := range setupAccounts(getenv) {
		if t != drivers.DefaultCRM {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	if _, ok := setupAccounts(getenv)[drivers.DefaultCRM]; ok {
		out = append([]string{drivers.DefaultCRM}, out...)
	}
	return out
}

// crmField is a setup flag of a CRM key: the flag's value and the CRM
// types that ask for the key.
type crmField struct {
	value *string
	field driver.SetupField
}

// crmFlags defines a flag for every crm key a registered CRM type asks
// for (one flag per name, shared by the types that ask for it).
func crmFlags(fs *flag.FlagSet) map[string]crmField {
	out := map[string]crmField{}
	types := slices.Sorted(maps.Keys(drivers.Accounts))
	for _, t := range types {
		spec, _ := driver.SetupFor(t)
		for _, f := range spec.Fields {
			if _, dup := out[f.Flag]; dup || f.Flag == "" {
				continue
			}
			usage := "CRM key " + f.Key
			if f.Env != "" {
				usage += " (env " + f.Env + ")"
			}
			out[f.Flag] = crmField{value: fs.String(f.Flag, "", usage), field: f}
		}
	}
	return out
}

// fillCRM takes the CRM type's keys from their flags, else the
// environment, and its secrets other than CRM_PASSWORD from the
// environment (a secret is never a flag).
func fillCRM(in *setup.Input, flags map[string]crmField, getenv func(string) string) {
	spec := in.Spec()
	in.CRM = map[string]string{}
	for _, f := range spec.Fields {
		v := ""
		if fl, ok := flags[f.Flag]; ok {
			v = *fl.value
		}
		if v == "" && f.Env != "" {
			v = getenv(f.Env)
		}
		if v = strings.TrimSpace(v); v != "" {
			in.CRM[f.Key] = v
		}
	}
	in.Secrets = map[string]string{}
	for _, x := range spec.Secrets {
		if x.Env != core.PasswordEnv {
			if v := getenv(x.Env); v != "" {
				in.Secrets[x.Env] = v
			}
		}
	}
}

// mirrorFlags define a flag for every key of a CRM an installation may
// hold as its second (core.MirrorTypes): the key's own flag with mirror-
// for crm-, e.g. --mirror-service-client.
func mirrorFlags(fs *flag.FlagSet) map[string]crmField {
	out := map[string]crmField{}
	for _, t := range core.MirrorTypes {
		spec, _ := driver.SetupFor(t)
		for _, f := range spec.Fields {
			name := "mirror-" + strings.TrimPrefix(f.Flag, "crm-")
			if _, dup := out[name]; dup || f.Flag == "" {
				continue
			}
			f.Flag = name
			out[name] = crmField{value: fs.String(name, "", "second CRM key "+f.Key), field: f}
		}
	}
	return out
}

// fillMirror takes the second CRM's keys from their flags.
func fillMirror(in *setup.Input, flags map[string]crmField) {
	if in.Mirror == "" {
		return
	}
	spec, _ := driver.SetupFor(in.Mirror)
	in.MirrorCRM = map[string]string{}
	for _, f := range spec.Fields {
		if fl, ok := flags["mirror-"+strings.TrimPrefix(f.Flag, "crm-")]; ok {
			if v := strings.TrimSpace(*fl.value); v != "" {
				in.MirrorCRM[f.Key] = v
			}
		}
	}
}

// askMirror asks whether the clubs also keep their bookings in a second
// CRM (Lunda): its type, or "" for none.
func askMirror(r *bufio.Reader, w io.Writer) (string, error) {
	for _, t := range core.MirrorTypes {
		spec, _ := driver.SetupFor(t)
		for {
			fmt.Fprintf(w, "Есть ли у клубов ещё и %s — вторая CRM, брони между ними будет сверять шлюз? (да/нет) [нет]: ", cmp.Or(spec.Name, t))
			s, err := r.ReadString('\n')
			if err != nil && s == "" {
				return "", fmt.Errorf("ввод прерван")
			}
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "да", "д", "yes", "y":
				return t, nil
			case "", "нет", "н", "no", "n":
			default:
				fmt.Fprintln(w, "Ответьте «да» или «нет»")
				continue
			}
			break
		}
	}
	return "", nil
}

// askCRM asks which CRM the club has.
func askCRM(r *bufio.Reader, w io.Writer, offered []string) (string, error) {
	var names []string
	for _, t := range offered {
		spec, _ := driver.SetupFor(t)
		names = append(names, t+" — "+spec.Name)
	}
	for {
		fmt.Fprintf(w, "CRM клуба (%s) [%s]: ", strings.Join(names, ", "), offered[0])
		s, err := r.ReadString('\n')
		if err != nil && s == "" {
			return "", fmt.Errorf("ввод прерван")
		}
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			return offered[0], nil
		}
		if slices.Contains(offered, s) {
			return s, nil
		}
		fmt.Fprintf(w, "Нет такой CRM: %s\n", s)
	}
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
	if r.Mirror != "" {
		spec, _ := driver.SetupFor(r.Mirror)
		fmt.Fprintf(w, "Вторая CRM: %s. Сверку броней между CRM оператор включает у клуба на хабе.\n", cmp.Or(spec.Name, r.Mirror))
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
		ServiceState: serviceState,
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

// serviceState asks this computer's service manager about the gateway's
// service, for doctor; a variable for tests.
var serviceState = func() service.State { return service.Query(runtime.GOOS, os.Getuid(), service.RunProbe) }

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
