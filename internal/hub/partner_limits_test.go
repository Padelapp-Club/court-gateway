package hub_test

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
)

// rawGateway connects a gateway with polls polls whose handler is h.
func rawGateway(t *testing.T, extraHub string, polls int, h http.Handler) *testkit.Hub {
	t.Helper()
	hb := testkit.StartHub(t, hubYAML+extraHub, hubEnv(), hub.Options{})
	secret := hb.Register("raw-club", "Raw")
	cfg := testkit.ClientConfig(hb.URL(), hb.Transport(), secret, h, nil)
	cfg.Polls = polls
	gw := testkit.StartGateway(t, cfg)
	testkit.WaitFor(t, "gateway connected", 10*time.Second, func() bool {
		return gw.Client.Connected() && hb.Server().Broker().Connected("raw-club")
	})
	return hb
}

// One partner may hold every poll of a gateway but one: its next request
// is refused at once (429), another partner still gets through, and a
// finished request frees its place.
func TestPartnerCannotHoldEveryPollOfAGateway(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 10)
	h := rawGateway(t, "", 3, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/clubs/raw-club/occupancy" {
			entered <- struct{}{}
			<-release
		}
		w.WriteHeader(http.StatusOK)
	}))
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free) // runs before the gateway stops: a failed check must not hang it
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { // not call: t.Fatal belongs to the test goroutine
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, h.URL()+"/v1/clubs/raw-club/occupancy", nil)
			req.Header.Set("Authorization", "Bearer "+contract.ReadKey)
			if resp, err := (&http.Client{Transport: h.Transport()}).Do(req); err == nil {
				resp.Body.Close()
			}
		}()
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("a slow request did not reach the gateway")
		}
	}
	if r := call(t, h, http.MethodGet, "/v1/clubs/raw-club", contract.ReadKey, ""); r.status != http.StatusTooManyRequests || r.json["code"] != "RATE_LIMITED" {
		t.Fatalf("a third request of the same partner: %d %s", r.status, r.body)
	}
	if r := call(t, h, http.MethodGet, "/v1/clubs/raw-club", contract.BookKey, ""); r.status != http.StatusOK {
		t.Fatalf("another partner: %d %s", r.status, r.body)
	}
	free()
	wg.Wait()
	if r := call(t, h, http.MethodGet, "/v1/clubs/raw-club", contract.ReadKey, ""); r.status != http.StatusOK {
		t.Fatalf("after the slow ones finished: %d %s", r.status, r.body)
	}
}

// Every partner answer is JSON by the hub's word, and a booking's
// Location is the path partners use — with the prefix of public_url.
func TestHubSetsContentTypeAndPrefixedLocation(t *testing.T) {
	gwHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Location", "/v1/clubs/raw-club/bookings/b-1")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"b-1"}`))
	})
	for prefix, extra := range map[string]string{"": "", "/gateway-hub": "public_url: https://hub.example.test/gateway-hub/\n"} {
		h := rawGateway(t, extra, 2, gwHandler)
		r := call(t, h, http.MethodPost, "/v1/clubs/raw-club/bookings", contract.BookKey, `{}`)
		if r.status != http.StatusCreated || r.header.Get("Content-Type") != "application/json; charset=utf-8" || r.header.Get("Location") != prefix+"/v1/clubs/raw-club/bookings/b-1" {
			t.Fatalf("prefix %q: %d %q %q", prefix, r.status, r.header.Get("Content-Type"), r.header.Get("Location"))
		}
	}
}
