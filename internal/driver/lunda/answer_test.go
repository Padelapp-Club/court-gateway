package lunda_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// An answer that is not a refusal of the session reads the same through
// a session call and a login-time one: a redirect is not followed
// (UpstreamRejected), a 5xx is UpstreamUnavailable, a 2xx that is not
// JSON is UpstreamInvalid.
func TestAnswersReadAlikeWithAndWithoutSession(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   driver.UpstreamKind
	}{
		"redirect": {http.StatusFound, ``, driver.UpstreamRejected},
		"5xx":      {http.StatusBadGateway, `{}`, driver.UpstreamUnavailable},
		"not JSON": {http.StatusOK, `<html>`, driver.UpstreamInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			answer := func(w http.ResponseWriter, r *http.Request) {
				if tc.status == http.StatusFound {
					w.Header().Set("Location", "https://elsewhere.test/")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}
			f, d := loggedIn(t) // with the session: booking-load
			intercept(f, "/club/booking/booking-load", answer)
			if _, err := d.Get(context.Background(), "l-00000000-0000-4000-8000-00000000dead"); upstreamKind(err) != tc.want {
				t.Fatalf("with the session: %v", err)
			}
			g := newAcctFixture(t) // without: qr/start
			intercept(g, "/auth/qr/start", answer)
			if _, err := g.account(t).StartLogin(context.Background(), driver.LoginQR, ""); upstreamKind(err) != tc.want {
				t.Fatalf("at login: %v", err)
			}
		})
	}
}
