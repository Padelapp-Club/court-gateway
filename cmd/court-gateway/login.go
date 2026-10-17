package main

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"rsc.io/qr"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/drivers"
)

// loginPoll is how often a QR login asks the CRM whether the person
// confirmed (the CRM's own admin asks every 3 s); a variable for tests.
var loginPoll = 3 * time.Second

// qrAttempts is how many QR codes are issued before a login gives up: one
// lives about a minute.
const qrAttempts = 5

// codeAttempts is how many codes may be typed before a login gives up.
const codeAttempts = 3

// cmdLogin logs the installation's interactive CRM account in again, when
// the CRM ended the session: a member of the club's staff scans a QR code
// or types a code the CRM sent. The running service takes the new session
// by itself.
func cmdLogin(args []string, stdout, stderr io.Writer, getenv func(string) string, defaultConfig string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfig, "gateway.yaml")
	method := fs.String("method", "", "qr or code (default: ask)")
	phone := fs.String("phone", "", "the staff member's phone for --method code, +7…")
	if fs.Parse(args) != nil {
		return 2
	}
	fail := func(err error) int { fmt.Fprintf(stderr, "login: %v\n", err); return 1 }
	env, err := core.EnvWithDotEnv(*cfgPath, getenv)
	if err != nil {
		return fail(err)
	}
	s, err := core.LoadFile(*cfgPath, env)
	if err != nil {
		return fail(err)
	}
	acct, err := core.BuildAccount(s, drivers.Accounts, s.Driver)
	if err != nil {
		return fail(err)
	}
	il, ok := acct.(driver.InteractiveLogin)
	if !ok {
		return fail(fmt.Errorf("в CRM %s входят паролем, а не сотрудником: если пароль сменился — court-gateway set-password", s.CRMType))
	}
	// the session is written in the state folder: closed to other users
	// first (Windows: protectSecrets; elsewhere the file is 0600 in 0700)
	if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
		return fail(err)
	}
	if protectSecrets != nil {
		if err := protectSecrets(s.StateDir); err != nil {
			return fail(fmt.Errorf("не удалось закрыть папку %s от других пользователей (запустите от имени администратора): %v", s.StateDir, err))
		}
	}
	ctx := context.Background()
	r := bufio.NewReader(stdin)
	if err := loginInteractively(ctx, il, r, stderr, driver.LoginMethod(*method), *phone, s.StateDir); err != nil {
		return fail(loginError(err))
	}
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := acct.CheckLogin(cctx); err != nil {
		return fail(fmt.Errorf("вход выполнен, но CRM не отвечает на проверку: %s", core.ReadyReason(err)))
	}
	fmt.Fprintln(stdout, "Вход в CRM выполнен. Служба шлюза подхватит его сама, перезапуск не нужен.")
	if cp, ok := acct.(driver.ClientPicker); ok && s.Driver.CRM[cp.ClientKey()] == nil {
		fmt.Fprintln(stdout, "Служебный клиент клуба не выбран: брони PadelApp не встанут. Выберите его: court-gateway setup --force с новым кодом подключения.")
	}
	return 0
}

// loginError is a login failure in the administrator's words.
func loginError(err error) error {
	switch {
	case errors.Is(err, driver.ErrLoginExpired):
		return errors.New("время входа истекло — запустите ещё раз")
	case errors.Is(err, driver.ErrLoginDeclined):
		return errors.New("вход отклонён в приложении CRM или код неверный")
	}
	return err
}

// loginInteractively runs an interactive login in a terminal: w is where
// the person reads (stderr), r where they type. method and phone, when
// given, skip their questions. A QR code is drawn in the terminal and also
// saved as a picture in dir (removed after the login), for a terminal that
// cannot show it. Neither the QR's text nor anything typed is logged.
func loginInteractively(ctx context.Context, il driver.InteractiveLogin, r *bufio.Reader, w io.Writer, method driver.LoginMethod, phone, dir string) error {
	methods := il.LoginMethods()
	if len(methods) == 0 {
		return errors.New("CRM не предлагает способа входа")
	}
	if method == "" {
		method = methods[0]
		if len(methods) > 1 {
			var names []string
			for i, m := range methods {
				names = append(names, fmt.Sprintf("%d — %s", i+1, methodName(m)))
			}
			s, err := ask(r, w, "Как войти в CRM: "+strings.Join(names, ", ")+" [1]")
			if err != nil {
				return err
			}
			if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= len(methods) {
				method = methods[n-1]
			}
		}
	}
	var st driver.LoginState
	var err error
	switch method {
	case driver.LoginQR:
		st, err = loginQR(ctx, il, w, dir)
	case driver.LoginCode:
		st, err = loginCode(ctx, il, r, w, phone)
	default:
		return fmt.Errorf("способ входа «%s» не поддерживается (qr, code)", method)
	}
	if err != nil {
		return err
	}
	return chooseClub(ctx, il, r, w, st.Clubs)
}

func methodName(m driver.LoginMethod) string {
	switch m {
	case driver.LoginQR:
		return "QR-код в приложении"
	case driver.LoginCode:
		return "телефон и код из сообщения"
	}
	return string(m)
}

func loginQR(ctx context.Context, il driver.InteractiveLogin, w io.Writer, dir string) (driver.LoginState, error) {
	png := filepath.Join(dir, "login-qr.png")
	defer os.Remove(png)
	for attempt := 1; ; attempt++ {
		ch, err := il.StartLogin(ctx, driver.LoginQR, "")
		if err != nil {
			return driver.LoginState{}, err
		}
		code, err := qr.Encode(ch.QR, qr.L)
		if err != nil {
			return driver.LoginState{}, fmt.Errorf("QR-код не построен: %v", err)
		}
		fmt.Fprintln(w, "Сотрудник клуба сканирует этот QR-код в приложении CRM и подтверждает вход («PadelApp — шлюз клуба»):")
		fmt.Fprint(w, terminalQR(code))
		if os.MkdirAll(dir, 0o700) == nil && core.WriteFileAtomic(png, code.PNG(), 0o600) == nil {
			fmt.Fprintf(w, "Если QR-код выше не читается, откройте картинку %s\n", png)
		}
		if !ch.Expires.IsZero() {
			fmt.Fprintf(w, "Код действует до %s. Жду подтверждения…\n", ch.Expires.Local().Format("15:04:05"))
		}
		st, err := pollQR(ctx, il)
		switch {
		case err == nil:
			return st, nil
		case errors.Is(err, driver.ErrLoginExpired) && attempt < qrAttempts:
			fmt.Fprintln(w, "QR-код истёк — вот новый.")
		default:
			return driver.LoginState{}, err
		}
	}
}

func pollQR(ctx context.Context, il driver.InteractiveLogin) (driver.LoginState, error) {
	t := time.NewTicker(loginPoll)
	defer t.Stop()
	for {
		st, err := il.PollLogin(ctx)
		if err != nil || st.Done {
			return st, err
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-t.C:
		}
	}
}

func loginCode(ctx context.Context, il driver.InteractiveLogin, r *bufio.Reader, w io.Writer, phone string) (driver.LoginState, error) {
	var err error
	if phone == "" {
		if phone, err = ask(r, w, "Телефон сотрудника клуба в CRM (+7…)"); err != nil {
			return driver.LoginState{}, err
		}
	}
	ch, err := il.StartLogin(ctx, driver.LoginCode, phone)
	if err != nil {
		return driver.LoginState{}, err
	}
	fmt.Fprintf(w, "Код отправлен %s.\n", cmp.Or(ch.Sent, "сотруднику"))
	for attempt := 1; ; attempt++ {
		code, err := ask(r, w, "Код")
		if err != nil {
			return driver.LoginState{}, err
		}
		st, err := il.ConfirmLogin(ctx, code)
		if errors.Is(err, driver.ErrLoginDeclined) && attempt < codeAttempts {
			fmt.Fprintln(w, "Код не подошёл, попробуйте ещё раз.")
			continue
		}
		return st, err
	}
}

// chooseClub selects the club of the session: the only one, or the one
// the person names.
func chooseClub(ctx context.Context, il driver.InteractiveLogin, r *bufio.Reader, w io.Writer, clubs []driver.Studio) error {
	switch len(clubs) {
	case 0:
		return nil
	case 1:
		fmt.Fprintf(w, "Клуб: %s\n", clubs[0].Name)
		return il.SelectClub(ctx, clubs[0].ID)
	}
	fmt.Fprintln(w, "Сотрудник работает в нескольких клубах. Какой клуб обслуживает этот шлюз?")
	for i, c := range clubs {
		fmt.Fprintf(w, "  %d) %s\n", i+1, c.Name)
	}
	for {
		s, err := ask(r, w, "Номер клуба")
		if err != nil {
			return err
		}
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= len(clubs) {
			return il.SelectClub(ctx, clubs[n-1].ID)
		}
	}
}

// pickClient has the person find and choose the club's service client
// (REQ-396); an empty answer chooses none.
func pickClient(r *bufio.Reader, w io.Writer) func(context.Context, driver.ClientPicker) (string, error) {
	return func(ctx context.Context, cp driver.ClientPicker) (string, error) {
		fmt.Fprintln(w, "Служебный клиент клуба для броней PadelApp: брони PadelApp встают в CRM на этого клиента (обычно клиент с именем клуба).")
		for {
			pattern, err := ask(r, w, "Найти клиента (имя или телефон; пусто — не выбирать сейчас)")
			if err != nil || pattern == "" {
				return "", err
			}
			found, err := cp.FindClients(ctx, pattern)
			if err != nil {
				return "", err
			}
			if len(found) == 0 {
				fmt.Fprintln(w, "Никого не нашлось.")
				continue
			}
			for i, c := range found {
				fmt.Fprintf(w, "  %d) %s", i+1, c.Name)
				if c.Hint != "" {
					fmt.Fprintf(w, " (%s)", c.Hint)
				}
				fmt.Fprintln(w)
			}
			s, err := ask(r, w, "Номер клиента (пусто — искать снова)")
			if err != nil {
				return "", err
			}
			if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= len(found) {
				return found[n-1].ID, nil
			}
		}
	}
}

func ask(r *bufio.Reader, w io.Writer, q string) (string, error) {
	fmt.Fprintf(w, "%s: ", q)
	s, err := r.ReadString('\n')
	if err != nil && s == "" {
		return "", errors.New("ввод прерван")
	}
	return strings.TrimSpace(s), nil
}

// terminalQR draws a QR code with half blocks, two modules a character
// high, dark on a light quiet zone of four modules (what phone cameras
// need on a dark terminal too).
func terminalQR(c *qr.Code) string {
	const quiet = 4
	dark := func(x, y int) bool {
		x, y = x-quiet, y-quiet
		return x >= 0 && y >= 0 && x < c.Size && y < c.Size && c.Black(x, y)
	}
	n := c.Size + 2*quiet
	var b strings.Builder
	for y := 0; y < n; y += 2 {
		for x := 0; x < n; x++ {
			top, bottom := dark(x, y), y+1 < n && dark(x, y+1)
			switch {
			case top && bottom:
				b.WriteRune(' ')
			case top:
				b.WriteRune('▄')
			case bottom:
				b.WriteRune('▀')
			default:
				b.WriteRune('█')
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}
