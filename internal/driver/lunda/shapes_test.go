package lunda_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda"
)

// The answers as Lunda gives them (seen live 2026-10-17): courts in
// result.infos (courtUid, title, number, categoryInfo); anything else —
// the shape the driver assumed before — does not read, and is never a
// club with no courts.
func TestCourtsAreReadFromInfos(t *testing.T) {
	f, d := loggedIn(t)
	if err := d.CheckConfig(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	for name, body := range map[string]string{
		"old shape":        `{"result":[{"uid":"` + court1 + `","name":"Корт 1"}]}`,
		"no infos":         `{"result":{"closureTypes":{}}}`,
		"court without id": `{"result":{"infos":[{"title":"Корт 1"}]}}`,
	} {
		f.stub.FailNext(http.MethodGet, "/court/settings/load-list", http.StatusOK, body)
		if err := d.CheckConfig(context.Background()); upstreamKind(err) != driver.UpstreamInvalid {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// Lunda's zone for the club (club/settings timeZoneId) not as far from UTC
// as the club's: the club is served, with a TIMEZONE_MISMATCH alert (and
// the missing service client's problem beside it, when there is one).
func TestZoneOfLundaIsChecked(t *testing.T) {
	f, d := loggedIn(t)
	f.stub.TimeZoneID = "Asia/Vladivostok"
	var ca *driver.ConfigAlert
	if err := d.CheckConfig(context.Background()); !errors.As(err, &ca) || ca.Code != "TIMEZONE_MISMATCH" || !strings.Contains(ca.Problems[0], "Asia/Vladivostok") {
		t.Fatalf("check: %v", err)
	}
	if err := d.Ready(context.Background()); !errors.As(err, &ca) || ca.Code != "TIMEZONE_MISMATCH" {
		t.Fatalf("ready: %v", err)
	}
	g := newFixture(t, fixtureOpts{noService: true})
	g.stub.TimeZoneID = "Asia/Vladivostok"
	if err := g.drv.CheckConfig(context.Background()); !errors.As(err, &ca) || ca.Code != "TIMEZONE_MISMATCH" || len(ca.Problems) != 2 {
		t.Fatalf("both: %v", err)
	}
	g.stub.TimeZoneID = "Europe/Moscow"
	if err := g.drv.CheckConfig(context.Background()); !errors.As(err, &ca) || ca.Code != "SERVICE_CLIENT_MISSING" {
		t.Fatalf("service client only: %v", err)
	}
	f.stub.FailNext(http.MethodGet, "/club/settings", http.StatusOK, `[]`)
	if err := d.CheckConfig(context.Background()); upstreamKind(err) != driver.UpstreamInvalid {
		t.Fatalf("settings that do not read: %v", err)
	}
}

// Visitors in result.items, the name one field; the old shape does not
// read.
func TestVisitorsAreReadFromItems(t *testing.T) {
	f := newAcctFixture(t)
	f.store.set(lunda.EncodeSession(f.stub.IssueSession(), clubUID, clubName))
	a := f.account(t)
	f.stub.FailNext(http.MethodPost, "/club/visitor/list", http.StatusOK, `{"result":[{"profileUid":"`+serviceUID+`","firstName":"Kosmos"}]}`)
	if _, err := a.FindClients(context.Background(), "kosmos"); upstreamKind(err) != driver.UpstreamInvalid {
		t.Fatalf("old shape: %v", err)
	}
}
