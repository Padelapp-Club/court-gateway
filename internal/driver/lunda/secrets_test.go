package lunda_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// No secret of a login or a session — the QR payload, the code, the
// temporal token, the phone, any token — ever reaches a log line or an
// error (an UpstreamError's Detail is logged), through a QR login, a code
// login, a renewal, a refused renewal, a lost session and failing calls.
func TestSecretsNeverLoggedNorInErrors(t *testing.T) {
	f := newAcctFixture(t)
	ctx := context.Background()
	var errs []error
	keep := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	secrets := map[string]string{}

	// QR: one expired, one done
	a := f.account(t)
	ch, err := a.StartLogin(ctx, "qr", "")
	keep(err)
	secrets["expired QR"] = ch.QR
	f.stub.ExpireQR()
	_, err = a.PollLogin(ctx)
	keep(err)
	ch = f.loginQR(t, a)
	secrets["QR"] = ch.QR
	secrets["first session"] = f.store.token()

	// code: a wrong code, then the right one
	b := f.account(t)
	_, err = b.StartLogin(ctx, "code", stubPhone)
	keep(err)
	push := f.stub.Calls()[len(f.stub.Calls())-1]
	_, err = b.ConfirmLogin(ctx, "135790")
	keep(err)
	st, err := b.ConfirmLogin(ctx, f.stub.Code)
	keep(err)
	keep(b.SelectClub(ctx, st.Clubs[0].ID))
	secrets["code"] = f.stub.Code
	secrets["wrong code"] = "135790"
	secrets["phone"] = "9990000000"
	for _, c := range f.stub.Calls() {
		if c.Path == "/profile/auth" {
			secrets["temporal token"] = c.Params["temporalToken"].(string)
		}
	}
	if push.Params["phone"] != secrets["phone"] {
		t.Fatalf("send-push: %+v", push.Params)
	}
	secrets["code session"] = f.store.token()

	// renewal, then a renewal refused, then calls on the lost session
	d := f.driver(t, b)
	f.stub.RequireRefresh()
	_, err = d.Occupancy(ctx, occQuery(futureDay()))
	keep(err)
	secrets["renewed session"] = f.store.token()
	f.stub.RequireRefresh()
	f.stub.FailNext(http.MethodPost, "/auth/refresh", http.StatusUnauthorized, `{"error":"`+secrets["renewed session"]+`"}`)
	_, err = d.Occupancy(ctx, occQuery(futureDay()))
	keep(err)
	_, err = d.Create(ctx, createReq(futureDay(), 10))
	keep(err)
	keep(d.Ready(ctx))
	keep(a.CheckLogin(ctx))
	f.stub.Revoke()
	keep(b.SelectClub(ctx, clubUID))
	_, err = d.Occupancy(ctx, occQuery(futureDay()))
	keep(err)
	// failing calls whose bodies echo a token
	f.store.set(nil)
	f.loginQR(t, f.account(t))
	secrets["last session"] = f.store.token()
	f.stub.FailNext(http.MethodGet, "/club/booking/slots-load", http.StatusBadRequest, `{"message":"`+secrets["last session"]+`"}`)
	_, err = d.Occupancy(ctx, occQuery(futureDay()))
	keep(err)
	f.stub.FailNext(http.MethodGet, "/club/booking/slots-load", http.StatusOK, `{"result":"`+secrets["last session"]+`"}`)
	_, err = d.Occupancy(ctx, occQuery(futureDay()))
	keep(err)

	if len(errs) < 7 {
		t.Fatalf("only %d errors collected: the check would prove little", len(errs))
	}
	// every token that went to Lunda, every temporal token and QR polled
	for i, c := range f.stub.Calls() {
		if c.Token != "" {
			secrets[fmt.Sprintf("token of call %d", i)] = c.Token
		}
		for _, k := range []string{"token", "temporalToken", "code", "phone"} {
			if v, _ := c.Params[k].(string); v != "" {
				secrets[fmt.Sprintf("%s of call %d", k, i)] = v
			}
		}
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "renewed") || !strings.Contains(logs, "refused") {
		t.Fatalf("the logs lack the renewal and the loss, so their check proves nothing: %s", logs)
	}
	for name, s := range secrets {
		if len(s) < 6 {
			t.Fatalf("secret %s is empty or too short to check", name)
		}
		if strings.Contains(logs, s) {
			t.Errorf("the logs hold the %s", name)
		}
		for _, err := range errs {
			if strings.Contains(err.Error(), s) {
				t.Errorf("an error holds the %s: %v", name, err)
			}
			var ue *driver.UpstreamError
			if errors.As(err, &ue) && strings.Contains(ue.Detail, s) {
				t.Errorf("an UpstreamError detail holds the %s", name)
			}
		}
	}
}
