package viva_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva/vivastub"
)

// A CRM address that redirects to plain HTTP gets neither the password
// (login) nor the access token (API): no request follows a redirect, and
// the redirect is an error.
func TestCRMRedirectsAreNotFollowed(t *testing.T) {
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits.Add(1)
		http.NotFound(w, r)
	}))
	defer plain.Close()
	stub := vivastub.New(stubUser, stubPass, studio)
	stub.AddRoom(room1, "Корт 1")
	for _, redirected := range []string{"/token", vivastub.APIPrefix + "/"} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, redirected) {
				http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusTemporaryRedirect)
				return
			}
			stub.ServeHTTP(w, r)
		}))
		d, err := viva.NewDriver(driver.Config{
			CRM:        driver.Params{"username": stubUser, "studio": studio, "auth_url": srv.URL + "/token", "api_url": srv.URL + vivastub.APIPrefix},
			Password:   stubPass,
			Courts:     []driver.CourtConfig{{ID: "court-1", Params: driver.Params{"viva_room": room1}}},
			HTTPClient: srv.Client(),
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = d.Get(context.Background(), "e-00000000-0000-4000-8000-0000000000e1")
		srv.Close()
		var ue *driver.UpstreamError
		if !errors.As(err, &ue) || !strings.Contains(ue.Detail, "redirect") {
			t.Fatalf("%s redirected: %v, want an upstream redirect error", redirected, err)
		}
	}
	if n := plainHits.Load(); n != 0 {
		t.Fatalf("%d requests followed the redirect to plain HTTP", n)
	}
}
