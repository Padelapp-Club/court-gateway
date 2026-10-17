package lunda

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// Account is the Lunda login of an installation: the session of a member
// of one club's staff. The club is the session's (SelectClub), so the
// account serves one Lunda club.
type Account struct {
	c       *client
	service string

	mu   sync.Mutex
	pend *pending // the login in progress
}

var (
	_ driver.Account          = (*Account)(nil)
	_ driver.InteractiveLogin = (*Account)(nil)
	_ driver.ClientPicker     = (*Account)(nil)
)

// pending is a login in progress. Nothing of it is ever logged: the QR
// payload, the temporal token, the phone and the login's token are all
// secrets of the person logging in.
type pending struct {
	method  driver.LoginMethod
	key     *keyPair // LoginQR: the key qr/start was sent
	qr      string   // LoginQR: the QR payload, polled by
	country string   // LoginCode
	phone   string   // LoginCode, national digits
	temp    string   // LoginCode: the temporal token of send-push
	expires time.Time
	token   string          // the login Lunda accepted, before a club is chosen
	clubs   []driver.Studio // the clubs it may act for
}

// Lifetimes of a login challenge, as observed: a QR is refused after about
// a minute (the admin offers a new one), a code after two.
const (
	qrLifetime   = time.Minute
	codeLifetime = 2 * time.Minute
)

// NewAccount is the driver.AccountFactory for crm.type "lunda". It reads
// the installation-level keys and takes up the saved session, if any.
func NewAccount(cfg driver.Config) (driver.Account, error) {
	var errs []string
	c, service := login(cfg, 30*time.Second, func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	})
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "\n"))
	}
	return &Account{c: c, service: service}, nil
}

// CheckLogin implements driver.Account: the session works.
func (a *Account) CheckLogin(ctx context.Context) error { return profileCurrent(ctx, a.c) }

// LoginMethods implements driver.InteractiveLogin: the admin's default
// tab is the QR code.
func (a *Account) LoginMethods() []driver.LoginMethod {
	return []driver.LoginMethod{driver.LoginQR, driver.LoginCode}
}

// HasSession implements driver.InteractiveLogin.
func (a *Account) HasSession() bool { return a.c.hasSession() }

// StartLogin implements driver.InteractiveLogin. LoginQR: qr/start with a
// fresh key pair; the QR encodes the token it answers. LoginCode:
// send-push to phone; Lunda sends the code to Telegram or as a push of
// its app.
func (a *Account) StartLogin(ctx context.Context, method driver.LoginMethod, phone string) (driver.Challenge, error) {
	switch method {
	case driver.LoginQR:
		key, err := newKeyPair()
		if err != nil {
			return driver.Challenge{}, err
		}
		status, res, err := a.c.exchange(ctx, opQRStart, "", map[string]any{"publicKey": key.public})
		if err != nil {
			return driver.Challenge{}, err
		}
		if status != http.StatusOK && status != http.StatusCreated {
			return driver.Challenge{}, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("POST /%s answered HTTP %d", opQRStart, status))
		}
		// [UNVERIFIED] the QR encodes result.token as it is (the admin
		// renders that string; read from its bundle, not decoded from a QR)
		var r struct {
			Token string `json:"token"`
		}
		if json.Unmarshal(res, &r) != nil || !validToken(r.Token) {
			return driver.Challenge{}, driver.Upstream(driver.UpstreamInvalid, "POST /"+opQRStart+": the answer has no QR token")
		}
		exp := a.c.now().Add(qrLifetime)
		a.setPending(&pending{method: method, key: key, qr: r.Token, expires: exp})
		return driver.Challenge{Method: method, QR: r.Token, Expires: exp}, nil
	case driver.LoginCode:
		country, national, err := splitPhone(phone)
		if err != nil {
			return driver.Challenge{}, err
		}
		status, res, err := a.c.exchange(ctx, opSendPush, "", map[string]any{"countryCode": country, "phone": national})
		if err != nil {
			return driver.Challenge{}, err
		}
		if status != http.StatusOK && status != http.StatusCreated {
			return driver.Challenge{}, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("POST /%s answered HTTP %d", opSendPush, status))
		}
		var r struct {
			TemporalToken string `json:"temporalToken"`
		}
		if json.Unmarshal(res, &r) != nil || !validToken(r.TemporalToken) {
			return driver.Challenge{}, driver.Upstream(driver.UpstreamInvalid, "POST /"+opSendPush+": the answer has no temporal token")
		}
		exp := a.c.now().Add(codeLifetime)
		a.setPending(&pending{method: method, country: country, phone: national, temp: r.TemporalToken, expires: exp})
		return driver.Challenge{Method: method, Sent: "в Telegram или пушем в приложение Lunda Padel", Expires: exp}, nil
	}
	return driver.Challenge{}, fmt.Errorf("lunda: no login method %q", method)
}

func (a *Account) setPending(p *pending) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pend = p
}

func (a *Account) pendingOf(method driver.LoginMethod) (*pending, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pend == nil || a.pend.method != method || a.pend.token != "" {
		return nil, fmt.Errorf("lunda: no %s login is waiting", method)
	}
	return a.pend, nil
}

// lundaClub is a club of a login's answer. [UNVERIFIED] the fields uid and
// name (the admin keeps the chosen one as ClubUid).
type lundaClub struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}

// loginAnswer is what a login answers when Lunda accepts it.
type loginAnswer struct {
	Status string      `json:"status"` // qr/status only
	Token  []string    `json:"token"`
	Clubs  []lundaClub `json:"clubs"`
}

// PollLogin implements driver.InteractiveLogin: qr/status as the admin
// polls it. PENDING waits; EXPIRED is driver.ErrLoginExpired; a declined
// login is driver.ErrLoginDeclined; SUCCESSFUL answers the login's token,
// encrypted with the key of qr/start, and the clubs.
func (a *Account) PollLogin(ctx context.Context) (driver.LoginState, error) {
	p, err := a.pendingOf(driver.LoginQR)
	if err != nil {
		return driver.LoginState{}, err
	}
	status, res, err := a.c.exchange(ctx, opQRStatus, "", map[string]any{"token": p.qr})
	if err != nil {
		return driver.LoginState{}, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return driver.LoginState{}, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("POST /%s answered HTTP %d", opQRStatus, status))
	}
	var r loginAnswer
	if json.Unmarshal(res, &r) != nil {
		return driver.LoginState{}, driver.Upstream(driver.UpstreamInvalid, "POST /"+opQRStatus+": the answer is not a login status")
	}
	switch strings.ToUpper(r.Status) {
	case "PENDING":
		return driver.LoginState{}, nil
	case "EXPIRED":
		a.setPending(nil)
		return driver.LoginState{}, driver.ErrLoginExpired
	// [UNVERIFIED] the status of a login the staff member declined (only
	// PENDING, SUCCESSFUL and EXPIRED were seen)
	case "DECLINED", "REJECTED", "CANCELED", "CANCELLED":
		a.setPending(nil)
		return driver.LoginState{}, driver.ErrLoginDeclined
	case "SUCCESSFUL":
		return a.accepted(p, p.key, r)
	}
	return driver.LoginState{}, driver.Upstream(driver.UpstreamInvalid, "POST /"+opQRStatus+": unknown login status")
}

// ConfirmLogin implements driver.InteractiveLogin: profile/auth with the
// code and a fresh key pair. A code Lunda refuses is
// driver.ErrLoginDeclined (it may be typed again while it lives); past its
// lifetime, driver.ErrLoginExpired.
func (a *Account) ConfirmLogin(ctx context.Context, code string) (driver.LoginState, error) {
	p, err := a.pendingOf(driver.LoginCode)
	if err != nil {
		return driver.LoginState{}, err
	}
	if !a.c.now().Before(p.expires) {
		a.setPending(nil)
		return driver.LoginState{}, driver.ErrLoginExpired
	}
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 12 || strings.IndexFunc(code, func(r rune) bool { return !unicode.IsDigit(r) }) >= 0 {
		return driver.LoginState{}, driver.ErrLoginDeclined
	}
	key, err := newKeyPair()
	if err != nil {
		return driver.LoginState{}, err
	}
	status, res, err := a.c.exchange(ctx, opAuth, "", map[string]any{
		"countryCode": p.country, "phone": p.phone, "temporalToken": p.temp, "code": code, "publicKey": key.public,
	})
	switch {
	case err != nil:
		return driver.LoginState{}, err
	case status == http.StatusBadRequest || status == http.StatusUnauthorized || status == http.StatusForbidden:
		return driver.LoginState{}, driver.ErrLoginDeclined
	case status == http.StatusGone:
		a.setPending(nil)
		return driver.LoginState{}, driver.ErrLoginExpired
	case status != http.StatusOK && status != http.StatusCreated:
		return driver.LoginState{}, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("POST /%s answered HTTP %d", opAuth, status))
	}
	var r loginAnswer
	if json.Unmarshal(res, &r) != nil {
		return driver.LoginState{}, driver.Upstream(driver.UpstreamInvalid, "POST /"+opAuth+": the answer is not a login")
	}
	return a.accepted(p, key, r)
}

// accepted takes the token of a login Lunda accepted and its clubs. The
// session is not usable (nor saved) before a club is chosen: Done with the
// clubs, then SelectClub — also for a single club, which the session is
// switched to all the same.
func (a *Account) accepted(p *pending, key *keyPair, r loginAnswer) (driver.LoginState, error) {
	tok, err := key.decrypt(r.Token)
	if err != nil {
		return driver.LoginState{}, driver.Upstream(driver.UpstreamInvalid, "the login's token: "+err.Error())
	}
	var clubs []driver.Studio
	for _, c := range r.Clubs {
		if uidRe.MatchString(c.UID) {
			clubs = append(clubs, driver.Studio{ID: c.UID, Name: strings.TrimSpace(c.Name)})
		}
	}
	if len(clubs) == 0 {
		a.setPending(nil)
		return driver.LoginState{}, driver.Upstream(driver.UpstreamRejected, "the staff member who logged in is staff of no club in Lunda")
	}
	a.mu.Lock()
	if a.pend == p {
		p.token, p.clubs, p.key = tok, clubs, nil
	}
	a.mu.Unlock()
	return driver.LoginState{Done: true, Clubs: clubs}, nil
}

// SelectClub implements driver.InteractiveLogin: profile/switch-club with a
// fresh key pair answers the session's token for the club; profile/current
// checks it, as the admin does; then it is saved. Without a login waiting
// for its club it switches the current session.
func (a *Account) SelectClub(ctx context.Context, id string) error {
	a.mu.Lock()
	p := a.pend
	a.mu.Unlock()
	if !uidRe.MatchString(id) {
		return fmt.Errorf("lunda: %q is not a club id", id)
	}
	var tok, name string
	if p != nil && p.token != "" {
		i := slices.IndexFunc(p.clubs, func(c driver.Studio) bool { return c.ID == id })
		if i < 0 {
			return fmt.Errorf("lunda: the login may not act for club %q", id)
		}
		tok, name = p.token, p.clubs[i].Name
	} else {
		s, err := a.c.current()
		if err != nil {
			return err
		}
		tok = s.Token
		if s.Club == id {
			name = s.ClubName
		}
	}
	key, err := newKeyPair()
	if err != nil {
		return err
	}
	status, res, err := a.c.exchange(ctx, opSwitchClub, tok, map[string]any{"uid": id, "publicKey": key.public})
	switch {
	case err != nil:
		return err
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return noSession(fmt.Sprintf("POST /%s: Lunda refused the login (HTTP %d)", opSwitchClub, status))
	case status != http.StatusOK && status != http.StatusCreated:
		return driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("POST /%s answered HTTP %d", opSwitchClub, status))
	}
	var r struct {
		Token []string `json:"token"`
	}
	if json.Unmarshal(res, &r) != nil {
		return driver.Upstream(driver.UpstreamInvalid, "POST /"+opSwitchClub+": the answer has no token")
	}
	clubTok, err := key.decrypt(r.Token)
	if err != nil {
		return driver.Upstream(driver.UpstreamInvalid, "the club's token: "+err.Error())
	}
	if status, _, err := a.c.exchange(ctx, opCurrent, clubTok, nil); err != nil {
		return err
	} else if status != http.StatusOK {
		return driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /%s with the club's session answered HTTP %d", opCurrent, status))
	}
	a.mu.Lock()
	if a.pend == p {
		a.pend = nil
	}
	a.mu.Unlock()
	return a.c.adopt(session{Token: clubTok, Club: id, ClubName: name})
}

// callingCodes are the two-digit country calling codes (ITU-T E.164);
// 1 and 7 are the one-digit ones, every other code has three digits.
var callingCodes = map[string]bool{
	"20": true, "27": true, "30": true, "31": true, "32": true, "33": true, "34": true, "36": true, "39": true,
	"40": true, "41": true, "43": true, "44": true, "45": true, "46": true, "47": true, "48": true, "49": true,
	"51": true, "52": true, "53": true, "54": true, "55": true, "56": true, "57": true, "58": true,
	"60": true, "61": true, "62": true, "63": true, "64": true, "65": true, "66": true,
	"81": true, "82": true, "84": true, "86": true, "90": true, "91": true, "92": true, "93": true, "94": true, "95": true, "98": true,
}

// splitPhone splits an international number (+7 999 123-45-67) into the
// country code and the national number, as the admin's form sends them.
// [UNVERIFIED] the form of countryCode: digits without "+" ("7").
func splitPhone(phone string) (country, national string, err error) {
	s := strings.TrimSpace(phone)
	if !strings.HasPrefix(s, "+") {
		return "", "", errors.New("телефон: укажите номер в международном виде, с «+» и кодом страны (+7…)")
	}
	digits := strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9':
			return r
		case r == ' ' || r == '-' || r == '(' || r == ')':
			return -1
		}
		return 'x'
	}, s[1:])
	if strings.Contains(digits, "x") || len(digits) < 8 || len(digits) > 15 {
		return "", "", errors.New("телефон: номер должен состоять из «+», кода страны и цифр номера (+7…)")
	}
	n := 3
	switch {
	case digits[0] == '1' || digits[0] == '7':
		n = 1
	case callingCodes[digits[:2]]:
		n = 2
	}
	return digits[:n], digits[n:], nil
}

// ClientKey implements driver.ClientPicker: the club's service client is
// the installation's crm.service_client.
func (a *Account) ClientKey() string { return ServiceClientKey }

// maxClients bounds the clients a search returns.
const maxClients = 50

// lundaVisitor is a client of the club as the visitor list shows it: only
// what the person choosing needs. [UNVERIFIED] the fields (profileUid is
// what visitor/add answers and a booking's customerUid takes).
type lundaVisitor struct {
	ProfileUID string `json:"profileUid"`
	FirstName  string `json:"firstName"`
	LastName   string `json:"lastName"`
	Phone      string `json:"phone"`
}

// FindClients implements driver.ClientPicker: the club's visitors matching
// pattern (name, phone or e-mail), as the admin's visitor search finds
// them (club/visitor/list, freeTextPattern) — the club's own clients,
// not every player of Lunda. Names and phones go to the person choosing
// only: never logged, the phone masked to its last digits.
func (a *Account) FindClients(ctx context.Context, pattern string) ([]driver.Client, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil, nil
	}
	status, res, err := a.c.call(ctx, opVisitors, nil, map[string]any{"freeTextPattern": pattern})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("POST /%s answered HTTP %d", opVisitors, status))
	}
	var list []lundaVisitor
	if decodeList(res, &list) != nil {
		return nil, driver.Upstream(driver.UpstreamInvalid, "POST /"+opVisitors+": the answer is not a list of visitors")
	}
	var out []driver.Client
	for _, v := range list {
		if !uidRe.MatchString(v.ProfileUID) {
			continue
		}
		out = append(out, driver.Client{ID: v.ProfileUID, Name: strings.TrimSpace(strings.TrimSpace(v.FirstName) + " " + strings.TrimSpace(v.LastName)), Hint: maskPhone(v.Phone)})
		if len(out) == maxClients {
			break
		}
	}
	return out, nil
}

// maskPhone keeps the last two digits of a phone: «телефон …67».
func maskPhone(p string) string {
	var digits []rune
	for _, r := range p {
		if r >= '0' && r <= '9' {
			digits = append(digits, r)
		}
	}
	if len(digits) < 6 {
		return ""
	}
	return "телефон …" + string(digits[len(digits)-2:])
}

// Discover implements driver.Account: the session's club is the one club
// of the account; its draft has every court (lunda_court, and lunda_tariff
// when the club has one tariff), the hours by weekday from the schedule
// of the next seven days, and the tariffs. The zone is not Lunda's to tell
// (its times have none): the club's own. No client is read.
func (a *Account) Discover(ctx context.Context, q driver.DiscoverQuery) (driver.Discovery, error) {
	var out driver.Discovery
	s, err := a.c.current()
	if err != nil {
		return out, err
	}
	out.Studios = []driver.Studio{{ID: s.Club, Name: s.ClubName}}
	if want := strings.TrimSpace(q.Studio); want != "" && want != s.Club {
		return out, driver.Upstream(driver.UpstreamRejected, "the gateway is logged in to another Lunda club")
	}
	out.Studio, out.ClubName = s.Club, s.ClubName
	out.Notices = append(out.Notices, driver.Notice{Code: "TIMEZONE_UNKNOWN",
		Message: "Лунда не сообщает часовой пояс клуба (её время — местное без пояса): укажите пояс клуба сами"})
	courts, err := courtList(ctx, a.c)
	if err != nil {
		return out, err
	}
	tariffs, err := tariffList(ctx, a.c)
	if err != nil {
		return out, err
	}
	for _, t := range tariffs {
		out.PriceLists = append(out.PriceLists, driver.PriceListInfo{ID: t.UID, Name: strings.TrimSpace(t.Name)})
	}
	for _, c := range courts {
		dc := driver.DiscoveredCourt{Name: strings.TrimSpace(c.Name), Params: driver.Params{CourtKey: c.uid()}}
		if len(tariffs) == 1 {
			dc.Params[TariffKey] = tariffs[0].UID
		}
		out.Courts = append(out.Courts, dc)
	}
	switch {
	case len(out.Courts) == 0:
		out.Notices = append(out.Notices, driver.Notice{Code: "NO_COURTS_FOUND", Message: "в клубе Лунды нет кортов: добавьте корты вручную"})
	case len(tariffs) == 0:
		out.Notices = append(out.Notices, driver.Notice{Code: "NO_PRICE_LISTS", Message: "в клубе Лунды нет тарифов: цены партнёрам будут недоступны"})
	case len(tariffs) > 1:
		out.Notices = append(out.Notices, driver.Notice{Code: "PRICE_LIST_AMBIGUOUS",
			Message: fmt.Sprintf("в клубе Лунды %d тарифов: впишите нужный каждому корту в %s", len(tariffs), TariffKey)})
	}
	out.Notices = append(out.Notices, driver.Notice{Code: "PRICE_CALENDAR",
		Message: "праздничные и предпраздничные дни календаря Лунды шлюз пока считает по дню недели: цены на них проверьте"})
	hours, err := a.hours(ctx)
	if err != nil {
		return out, err
	}
	out.Hours = hours
	if hours == ([7]*[2]int{}) {
		out.Notices = append(out.Notices, driver.Notice{Code: "HOURS_UNKNOWN", Message: "расписание Лунды не показало часов работы: впишите их вручную"})
	}
	return out, nil
}

// hours reads the schedule of the next seven days for their hours
// (availableHours): opening at the first hour, closing after the last.
// [UNVERIFIED] that availableHours lists the hours a booking may start in
// (8..22 for a club open 08:00–23:00).
func (a *Account) hours(ctx context.Context) ([7]*[2]int, error) {
	var out [7]*[2]int
	today := localDay(a.c.now(), time.Local)
	for i := 0; i < 7; i++ {
		day := today.AddDate(0, 0, i)
		ds, err := readSlots(ctx, a.c, day)
		if err != nil {
			return out, err
		}
		if len(ds.AvailableHours) == 0 {
			continue
		}
		lo, hi := ds.AvailableHours[0], ds.AvailableHours[0]
		for _, h := range ds.AvailableHours {
			lo, hi = min(lo, h), max(hi, h)
		}
		if lo < 0 || hi > 23 {
			continue
		}
		wd := (int(day.Weekday()) + 6) % 7
		out[wd] = &[2]int{lo * 60, (hi + 1) * 60}
	}
	return out, nil
}
