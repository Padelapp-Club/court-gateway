package fitness1c_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/fitness1c"
	"github.com/Padelapp-Club/court-gateway/internal/driver/fitness1c/fitness1cstub"
)

var ctx = context.Background()

// day is local midnight in Moscow three days ahead.
func day() time.Time {
	n := time.Now().In(moscow).AddDate(0, 0, 3)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, moscow)
}

func at(d time.Time, hh, mm int) time.Time { return d.Add(time.Duration(hh*60+mm) * time.Minute) }

func upstreamKind(t *testing.T, err error, want driver.UpstreamKind) {
	t.Helper()
	var ue *driver.UpstreamError
	if !errors.As(err, &ue) || ue.Kind != want {
		t.Fatalf("error = %v, want upstream kind %d", err, want)
	}
}

func create(f *fixture, court string, start, end time.Time) (driver.Booking, error) {
	return f.drv.Create(ctx, driver.CreateRequest{Court: court, Start: start, End: end, Kind: driver.KindGame, Partner: partner})
}

// A service that needs a payment basis gives a 15-minute reservation, not a
// booking: it is removed and the create refused (no half-booking).
func TestCreateOfATemporaryReservationIsUndone(t *testing.T) {
	f := newFixture(t, fixtureOpts{services: map[string]map[string]any{"booking": {"service": gridSvc}}})
	d := day()
	_, err := create(f, "court-1", at(d, 10, 0), at(d, 11, 0))
	upstreamKind(t, err, driver.UpstreamRejected)
	if left := f.stub.Rentals(); len(left) != 0 {
		t.Fatalf("rentals left in 1C: %v", left)
	}
	if got := f.stub.Calls("DELETE /rental/"); got != 1 {
		t.Fatalf("DELETE /rental/ calls = %d, want 1", got)
	}
}

// The removal is tried three times; the last failure leaves an error with
// the rental's id in the log for removal by hand.
func TestCompensationTriesThreeTimes(t *testing.T) {
	f := newFixture(t, fixtureOpts{services: map[string]map[string]any{"booking": {"service": gridSvc}}})
	f.stub.Fail("DELETE /rental/", fitness1cstub.Failure{Status: 500}, fitness1cstub.Failure{Status: 500}, fitness1cstub.Failure{Status: 500})
	d := day()
	if _, err := create(f, "court-1", at(d, 10, 0), at(d, 11, 0)); err == nil {
		t.Fatal("create succeeded")
	}
	if got := f.stub.Calls("DELETE /rental/"); got != 3 {
		t.Fatalf("DELETE /rental/ calls = %d, want 3", got)
	}
	left := f.stub.Rentals()
	if len(left) != 1 || !strings.Contains(f.built.Logs.String(), "remove it by hand") || !strings.Contains(f.built.Logs.String(), left[0]) {
		t.Fatalf("no error naming the rental left (%v): %s", left, f.built.Logs.String())
	}
}

// 1060 is a taken slot (a club's rental the gateway's check did not see).
func TestCreateOnATakenRoomIsSlotUnavailable(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	d := day()
	f.stub.SeedRental(room1, at(d, 10, 0), at(d, 11, 0), false, clubPrivate)
	_, err := create(f, "court-1", at(d, 10, 30), at(d, 11, 30))
	if !errors.Is(err, driver.ErrSlotUnavailable) {
		t.Fatalf("error = %v, want ErrSlotUnavailable", err)
	}
}

// An answer lost on the way back: the POST is sent once more and 1C,
// idempotent per client and slot, gives the same rental (one, not two).
func TestCreateRetriesALostAnswerOnce(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.stub.Fail("POST /rental/", fitness1cstub.Failure{Apply: true})
	d := day()
	b, err := create(f, "court-1", at(d, 10, 0), at(d, 11, 0))
	if err != nil {
		t.Fatal(err)
	}
	left := f.stub.Rentals()
	if len(left) != 1 || b.ID != "f-"+left[0] || f.stub.Calls("POST /rental/") != 2 {
		t.Fatalf("booking %s, rentals %v, posts %d", b.ID, left, f.stub.Calls("POST /rental/"))
	}
}

// A refused client token (4003) is issued again once.
func TestAStaleClientTokenIsIssuedAgain(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	d := day()
	if _, err := create(f, "court-1", at(d, 10, 0), at(d, 11, 0)); err != nil {
		t.Fatal(err)
	}
	f.stub.RevokeTokens()
	if _, err := create(f, "court-2", at(d, 10, 0), at(d, 11, 0)); err != nil {
		t.Fatalf("after a stale token: %v", err)
	}
	if got := f.stub.Calls("GET /client_usertoken/"); got != 2 {
		t.Fatalf("tokens issued = %d, want 2", got)
	}
}

// Writes need a booking service and the technical client's partner.
func TestWritesNeedAServiceAndThePartner(t *testing.T) {
	d := day()
	f := newFixture(t, fixtureOpts{services: map[string]map[string]any{}})
	if _, err := create(f, "court-1", at(d, 10, 0), at(d, 11, 0)); !errors.Is(err, driver.ErrNotSupported) {
		t.Fatalf("no service: %v", err)
	}
	f = newFixture(t, fixtureOpts{})
	_, err := f.drv.Create(ctx, driver.CreateRequest{Court: "court-1", Start: at(d, 10, 0), End: at(d, 11, 0), Kind: driver.KindGame, Partner: "other"})
	if !errors.Is(err, driver.ErrNotSupported) || len(f.stub.Rentals()) != 0 {
		t.Fatalf("another partner: %v, rentals %v", err, f.stub.Rentals())
	}
}

// A rental cancelled at the club already counts as cancelled; a club's
// rental is never ours, even by its 1C id.
func TestCancelOwnership(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	d := day()
	b, err := create(f, "court-1", at(d, 10, 0), at(d, 11, 0))
	if err != nil {
		t.Fatal(err)
	}
	f.stub.CancelRental(strings.TrimPrefix(b.ID, "f-"))
	if err := f.drv.Cancel(ctx, b.ID, partner); err != nil {
		t.Fatalf("cancel of a rental the club cancelled: %v", err)
	}
	club := f.stub.SeedRental(room1, at(d, 14, 0), at(d, 15, 0), false, clubPrivate)
	if err := f.drv.Cancel(ctx, "f-"+club, partner); !errors.Is(err, driver.ErrNotOwned) {
		t.Fatalf("cancel of a club rental by its id: %v", err)
	}
	if len(f.stub.Rentals()) != 1 || f.stub.Calls("DELETE /rental/") != 0 {
		t.Fatalf("the club's rental was touched: %v", f.stub.Rentals())
	}
}

// 1C's refusal to read a rental is never "not found": a booking reported
// gone is cancelled by the partner with a refund.
func TestGetNeverTurnsARefusalIntoNotFound(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	d := day()
	b, err := create(f, "court-1", at(d, 10, 0), at(d, 11, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, fl := range []fitness1cstub.Failure{{Code: 1100}, {Code: 4003}, {Status: 404}, {Status: 500}} {
		f.stub.Fail("GET /rental/", fl)
		if _, err := f.drv.Get(ctx, b.ID); err == nil || errors.Is(err, driver.ErrNotFound) {
			t.Fatalf("%+v read as %v", fl, err)
		}
	}
	got, err := f.drv.Get(ctx, b.ID)
	if err != nil || got.Status != driver.StatusActive || !got.Start.Equal(at(d, 10, 0)) || got.Court != "court-1" {
		t.Fatalf("get = %+v, %v", got, err)
	}
}

func occupancy(t *testing.T, f *fixture, from, to time.Time, courts ...string) driver.Occupancy {
	t.Helper()
	if len(courts) == 0 {
		courts = []string{"court-1", "court-2"}
	}
	o, err := f.drv.Occupancy(ctx, driver.OccupancyQuery{Courts: courts, From: from, To: to})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// Outside 1C's grid every half-hour is busy (D14: a slot outside 1C's
// hours is not sold); a block that is no rental is busy too.
func TestOutsideTheGridIsBusy(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.stub.FirstSlot, f.stub.LastSlot = 7*60, 22*60+30
	d := day()
	f.stub.SeedBlock(room1, at(d, 12, 0), at(d, 13, 0))
	o := occupancy(t, f, d, d.AddDate(0, 0, 1), "court-1")
	want := []string{
		stretchID(room1, at(d, 0, 0), at(d, 7, 0)),
		stretchID(room1, at(d, 12, 0), at(d, 13, 0)),
		stretchID(room1, at(d, 23, 0), at(d, 24, 0)),
	}
	var got []string
	for _, iv := range o.Intervals {
		got = append(got, iv.ID)
	}
	if strings.Join(got, " ") != strings.Join(want, " ") || !o.Complete {
		t.Fatalf("intervals %v (complete %v), want %v", got, o.Complete, want)
	}
}

// What cannot be read is never free: a room missing from the grid, an
// untrusted slot, one failed day of several each fail the whole call.
func TestUnreadableGridIsNeverFree(t *testing.T) {
	d := day()
	t.Run("room missing", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		f.stub.Fail("GET /rental_rooms_times/", fitness1cstub.Failure{Body: `{"result":true,"data":[{"room":{"id":"` + room2 + `"},"durations":[]}]}`})
		_, err := f.drv.Occupancy(ctx, driver.OccupancyQuery{Courts: []string{"court-1"}, From: at(d, 10, 0), To: at(d, 11, 0)})
		upstreamKind(t, err, driver.UpstreamInvalid)
	})
	t.Run("untrusted slot", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		f.stub.Fail("GET /rental_rooms_times/", fitness1cstub.Failure{Body: `{"result":true,"data":[{"room":{"id":"` + room1 + `"},"durations":[{"time":"10:00","date_time":"` + at(d, 10, 0).Format("2006-01-02") + ` 10:00"}]}]}`})
		_, err := f.drv.Occupancy(ctx, driver.OccupancyQuery{Courts: []string{"court-1"}, From: at(d, 10, 0), To: at(d, 11, 0)})
		upstreamKind(t, err, driver.UpstreamInvalid)
	})
	t.Run("one day of three", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		f.stub.Fail("GET /rental_rooms_times/", fitness1cstub.Failure{}, fitness1cstub.Failure{Status: 503})
		f.stub.Fail("GET /rental_rooms_times/") // nothing more
		_, err := f.drv.Occupancy(ctx, driver.OccupancyQuery{Courts: []string{"court-1"}, From: d, To: d.AddDate(0, 0, 2)})
		if err == nil {
			t.Fatal("occupancy with a failed day succeeded")
		}
	})
}

// Own rentals not all read (1C reports more than were listed): the answer
// is incomplete, so the gateway refuses to book on it.
func TestUnlistedOwnRentalsMakeOccupancyIncomplete(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.stub.Fail("GET /appointments/", fitness1cstub.Failure{Body: `{"result":true,"data":[],"pagination_response":{"requested_offset":0,"page_size":30,"total_results":45}}`})
	d := day()
	if o := occupancy(t, f, at(d, 10, 0), at(d, 11, 0)); o.Complete {
		t.Fatal("occupancy complete with 45 appointments reported and none read")
	}
}

// A refused login stops requests for a while (a stream of refused logins
// may lock the 1C user); a new password ends the stop at once.
func TestARefusedLoginPauses(t *testing.T) {
	f := newFixture(t, fixtureOpts{env: map[string]string{"CRM_PASSWORD": "wrong"}})
	d := day()
	_, err := f.drv.Occupancy(ctx, driver.OccupancyQuery{Courts: []string{"court-1"}, From: d, To: at(d, 1, 0)})
	upstreamKind(t, err, driver.UpstreamAuth)
	before := f.stub.Calls("GET /clubs/")
	_, err = f.drv.Occupancy(ctx, driver.OccupancyQuery{Courts: []string{"court-1"}, From: d, To: at(d, 1, 0)})
	upstreamKind(t, err, driver.UpstreamAuth)
	if after := f.stub.Calls("GET /clubs/"); after != before {
		t.Fatalf("requests during the pause: %d", after-before)
	}
	acct, err := fitness1c.NewAccount(driver.Config{CRM: driver.Params{"base_url": f.srv.URL, "username": stubUser},
		Password: "wrong", Secrets: map[string]string{fitness1c.APIKeyEnv: stubKey}, HTTPClient: f.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	upstreamKind(t, acct.CheckLogin(ctx), driver.UpstreamAuth)
	ps := acct.(driver.PasswordSetter)
	if !ps.PasswordRejected() || ps.NextLogin().IsZero() {
		t.Fatal("the account does not report the refused login")
	}
	ps.SetPassword(stubPass)
	if err := acct.CheckLogin(ctx); err != nil || ps.PasswordRejected() {
		t.Fatalf("after a new password: %v", err)
	}
}

// Neither the 1C login nor the client's phone or token ever reaches a log.
func TestSecretsStayOutOfLogs(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	d := day()
	b, err := create(f, "court-1", at(d, 10, 0), at(d, 11, 0))
	if err != nil {
		t.Fatal(err)
	}
	_ = f.drv.Cancel(ctx, b.ID, partner)
	f.stub.Fail("GET /clubs/", fitness1cstub.Failure{Status: 401})
	_ = f.drv.Ready(ctx)
	logs := f.built.Logs.String()
	if logs == "" {
		t.Fatal("nothing logged: the check proves nothing")
	}
	for _, s := range []string{stubPass, stubKey, clientPhone, strings.TrimPrefix(clientPhone, "+")} {
		if strings.Contains(logs, s) {
			t.Fatalf("logs leak %q", s)
		}
	}
}

// CheckConfig: a booking service that needs a payment basis, a room that
// is not the club's, another zone.
func TestCheckConfig(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	if err := f.drv.CheckConfig(ctx); err != nil {
		t.Fatalf("a matching config: %v", err)
	}
	f = newFixture(t, fixtureOpts{services: map[string]map[string]any{"booking": {"service": gridSvc}}})
	var me *driver.MisconfigError
	if err := f.drv.CheckConfig(ctx); !errors.As(err, &me) || !strings.Contains(err.Error(), "основания оплаты") {
		t.Fatalf("a paid booking service: %v", err)
	}
	f = newFixture(t, fixtureOpts{timezone: "Asia/Vladivostok"})
	var ca *driver.ConfigAlert
	if err := f.drv.CheckConfig(ctx); !errors.As(err, &ca) || ca.Code != "TIMEZONE_MISMATCH" {
		t.Fatalf("another zone: %v", err)
	}
	stub := newStub()
	srv := stubServer(t, stub)
	spec := testClub(fixtureOpts{})
	spec.Courts[1].Params[fitness1c.RoomKey] = "00000000-0000-4000-8000-0000000001ff"
	b := contract.Build(t, crmYAML(srv.URL, ""), spec, installEnv(fixtureOpts{}), "fitness1c", fitness1c.New, func(c *driver.Config) { c.HTTPClient = srv.Client() })
	if err := b.Driver.(*fitness1c.Driver).CheckConfig(ctx); !errors.As(err, &me) || !strings.Contains(err.Error(), "courts[1]") {
		t.Fatalf("a room not the club's: %v", err)
	}
}

// The installation's config: the key, the password, the phone with its partner.
func TestConfigErrors(t *testing.T) {
	base := func() driver.Config {
		return driver.Config{
			CRM:      driver.Params{"base_url": "https://1c.example.test/club", "username": stubUser, "studio": club1C},
			Password: stubPass, Secrets: map[string]string{fitness1c.APIKeyEnv: stubKey},
			Courts: []driver.CourtConfig{{ID: "c1", Params: driver.Params{fitness1c.RoomKey: room1}}},
		}
	}
	if _, err := fitness1c.New(base()); err != nil {
		t.Fatalf("a valid config: %v", err)
	}
	for name, mutate := range map[string]func(*driver.Config){
		"no key":            func(c *driver.Config) { c.Secrets = nil },
		"no password":       func(c *driver.Config) { c.Password = "" },
		"http":              func(c *driver.Config) { c.CRM["base_url"] = "http://1c.example.test" },
		"phone, no partner": func(c *driver.Config) { c.CRM["client_phone"] = clientPhone },
		"bad phone":         func(c *driver.Config) { c.CRM["client_phone"], c.CRM["partner"] = "12", partner },
		"room not an id":    func(c *driver.Config) { c.Courts[0].Params[fitness1c.RoomKey] = "Корт 1" },
		"unknown key":       func(c *driver.Config) { c.CRM["break_title"] = "x" },
		"service not an id": func(c *driver.Config) { c.Services = map[string]driver.Params{"booking": {"service": "PadelApp"}} },
		"unknown service":   func(c *driver.Config) { c.Services = map[string]driver.Params{"party": {"service": padelSvc}} },
		"studio missing":    func(c *driver.Config) { delete(c.CRM, "studio") },
		"two courts one room": func(c *driver.Config) {
			c.Courts = append(c.Courts, driver.CourtConfig{ID: "c2", Params: driver.Params{fitness1c.RoomKey: room1}})
		},
	} {
		c := base()
		mutate(&c)
		if _, err := fitness1c.New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Prices: 875 a 30-minute seance is 1750 an hour, all day, in 1C's zone;
// crm.price_service picks another service.
func TestPriceTable(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	pt, err := f.drv.PriceTable(ctx, "court-1")
	if err != nil || len(pt.Base) != 1 || pt.Base[0].PerHour != 175000 || pt.Base[0].From != 0 || pt.Base[0].To != 1440 || pt.Zone.String() != "Europe/Moscow" {
		t.Fatalf("price table %+v, %v", pt, err)
	}
	stub := newStub()
	stub.AddService(fitness1cstub.Service{ID: "00000000-0000-4000-8000-0000000001c4", Title: "Вечер", Seance: 60, Price: "2400.50"})
	srv := stubServer(t, stub)
	spec := testClub(fixtureOpts{clubCRM: map[string]any{"price_service": "00000000-0000-4000-8000-0000000001c4"}})
	b := contract.Build(t, crmYAML(srv.URL, ""), spec, installEnv(fixtureOpts{}), "fitness1c", fitness1c.New, func(c *driver.Config) { c.HTTPClient = srv.Client() })
	pt, err = b.Driver.PriceTable(ctx, "court-1")
	if err != nil || pt.Base[0].PerHour != 240050 {
		t.Fatalf("price service: %+v, %v", pt, err)
	}
}

// Discovery drafts a club: rooms as courts, 1C's zone, hours from the grid,
// the service bookings can be made by.
func TestDiscover(t *testing.T) {
	stub := newStub()
	stub.FirstSlot, stub.LastSlot = 7*60, 22*60+30
	srv := stubServer(t, stub)
	acct, err := fitness1c.NewAccount(driver.Config{CRM: driver.Params{"base_url": srv.URL, "username": stubUser},
		Password: stubPass, Secrets: map[string]string{fitness1c.APIKeyEnv: stubKey}, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ds, err := acct.Discover(ctx, driver.DiscoverQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if ds.Studio != club1C || ds.Timezone != "Europe/Moscow" || len(ds.Courts) != 3 || ds.Courts[0].Params[fitness1c.RoomKey] != room1 {
		t.Fatalf("discovery %+v", ds)
	}
	for wd, h := range ds.Hours {
		if h == nil || h[0] != 7*60 || h[1] != 23*60 {
			t.Fatalf("hours of %d: %v", wd, h)
		}
	}
	var codes []string
	for _, n := range ds.Notices {
		codes = append(codes, n.Code)
		if n.Code == "BOOKING_SERVICE" && !strings.Contains(n.Message, padelSvc) {
			t.Fatalf("booking service notice %q", n.Message)
		}
	}
	if !strings.Contains(strings.Join(codes, " "), "BOOKING_SERVICE") {
		t.Fatalf("notices %v", codes)
	}
}

// The club cancels between the gateway's read and its DELETE: 1037
// "already cancelled" is the cancel done.
func TestCancelRacingTheClubIsDone(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	d := day()
	b, err := create(f, "court-1", at(d, 10, 0), at(d, 11, 0))
	if err != nil {
		t.Fatal(err)
	}
	f.stub.Fail("DELETE /rental/", fitness1cstub.Failure{Code: 1037})
	if err := f.drv.Cancel(ctx, b.ID, partner); err != nil {
		t.Fatalf("cancel answered 1037: %v", err)
	}
}

// Ownership cannot be settled while the technical client's appointments
// were not all read: no NOT_OWNED for what may be ours.
func TestCancelWithUnlistedAppointmentsIsNotNotOwned(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	d := day()
	club := f.stub.SeedRental(room1, at(d, 14, 0), at(d, 15, 0), false, clubPrivate)
	f.stub.Fail("GET /appointments/", fitness1cstub.Failure{Body: `{"result":true,"data":[],"pagination_response":{"requested_offset":0,"page_size":30,"total_results":45}}`})
	err := f.drv.Cancel(ctx, "f-"+club, partner)
	if err == nil || errors.Is(err, driver.ErrNotOwned) {
		t.Fatalf("cancel with an incomplete listing: %v", err)
	}
	if len(f.stub.Rentals()) != 1 {
		t.Fatal("the club's rental was touched")
	}
}

// A rental service whose step is not a whole number of minutes makes the
// services unusable, as in the backend adapter.
func TestServicesWithoutAWholeStepAreRefused(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.stub.Fail("GET /rental_services/", fitness1cstub.Failure{Body: `{"result":true,"data":[{"id":"` + gridSvc + `","seance_length":30.5,"price":"875","payment_required":true}]}`})
	d := day()
	_, err := f.drv.Occupancy(ctx, driver.OccupancyQuery{Courts: []string{"court-1"}, From: at(d, 10, 0), To: at(d, 11, 0)})
	upstreamKind(t, err, driver.UpstreamInvalid)
}

// A 1C too slow for the request's deadline: the error says so without the
// URL, whose query carries the technical client's phone.
func TestATimeoutDoesNotCarryThePhone(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.stub.Fail("GET /client_usertoken/", fitness1cstub.Failure{Delay: 2 * time.Second})
	c, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	d := day()
	_, err := f.drv.Create(c, driver.CreateRequest{Court: "court-1", Start: at(d, 10, 0), End: at(d, 11, 0), Kind: driver.KindGame, Partner: partner})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want a deadline", err)
	}
	for _, s := range []string{strings.TrimPrefix(clientPhone, "+"), "phone="} {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("the error carries %q: %v", s, err)
		}
	}
}

// The club moves the gateway's rental off the grid (10:15–11:15) next to
// a class at 10:00–10:15: the 10:00 half-hour is only partly the gateway's,
// so it stays busy (clean-room round 2).
func TestAPartlyOwnHalfHourStaysBusy(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	d := day()
	f.stub.SeedClientRental(clientPhone, room1, padelSvc, at(d, 10, 15), at(d, 11, 15))
	f.stub.SeedBlock(room1, at(d, 10, 0), at(d, 10, 15))
	if o := occupancy(t, f, at(d, 10, 0), at(d, 10, 15), "court-1"); len(o.Intervals) == 0 {
		t.Fatalf("10:00–10:15, taken by a class, reads free: %+v", o)
	}
}

// The club moves the gateway's rental to a room no court of this club is
// on, under another service: it is not gone (clean-room round 2).
func TestAMovedOwnRentalIsNotGone(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	d := day()
	b, err := create(f, "court-1", at(d, 10, 0), at(d, 11, 0))
	if err != nil {
		t.Fatal(err)
	}
	f.stub.MoveRental(strings.TrimPrefix(b.ID, "f-"), roomOther, gridSvc, at(d, 12, 0), at(d, 13, 0))
	if _, err := f.drv.Get(ctx, b.ID); !errors.Is(err, driver.ErrCourtNotConfigured) {
		t.Fatalf("get of a moved rental: %v, want ErrCourtNotConfigured", err)
	}
}

// A rental of the gateway that has ended is no longer listed as planned:
// cancelling it is BOOKING_STARTED, not NOT_OWNED (clean-room round 2).
func TestCancelOfAnEndedRentalIsStarted(t *testing.T) {
	clock := newClock()
	f := newFixture(t, fixtureOpts{now: clock.Now})
	d := day()
	b, err := create(f, "court-1", at(d, 10, 0), at(d, 11, 0))
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(at(d, 12, 0).Sub(clock.Now()))
	if err := f.drv.Cancel(ctx, b.ID, partner); !errors.Is(err, driver.ErrStarted) {
		t.Fatalf("cancel of an ended rental: %v, want ErrStarted", err)
	}
	if len(f.stub.Rentals()) != 1 {
		t.Fatal("the ended rental was cancelled")
	}
}

// A booking service with a price would leave the technical client a debt
// at the club for every partner booking (clean-room round 2).
func TestCheckConfigRefusesAPricedBookingService(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.stub.Fail("GET /rental_services/", fitness1cstub.Failure{Body: `{"result":true,"data":[{"id":"` + gridSvc + `","seance_length":30,"price":"875","payment_required":true},{"id":"` + padelSvc + `","seance_length":30,"price":"875","payment_required":false}]}`})
	var me *driver.MisconfigError
	if err := f.drv.CheckConfig(ctx); !errors.As(err, &me) || !strings.Contains(err.Error(), "цена не 0") {
		t.Fatalf("a priced booking service: %v", err)
	}
}
