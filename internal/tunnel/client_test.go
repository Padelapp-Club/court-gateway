package tunnel

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestBackoffGrowsWithJitterAndCaps(t *testing.T) {
	c, err := NewClient(ClientConfig{HubURL: "https://hub.invalid", Secret: "s", Handler: http.NotFoundHandler(),
		MinBackoff: 100 * time.Millisecond, MaxBackoff: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for failures := 1; failures < 20; failures++ {
		ceil := 100 * time.Millisecond << (failures - 1)
		if ceil > 2*time.Second || ceil <= 0 {
			ceil = 2 * time.Second
		}
		for i := 0; i < 50; i++ {
			d := c.backoff(failures, false)
			if d < 50*time.Millisecond || d >= 50*time.Millisecond+ceil {
				t.Fatalf("failures=%d: %v outside [50ms, %v)", failures, d, 50*time.Millisecond+ceil)
			}
		}
	}
	seen := map[time.Duration]bool{}
	for i := 0; i < 20; i++ {
		seen[c.backoff(5, false)] = true
	}
	if len(seen) < 5 {
		t.Fatal("no jitter: reconnects of a fleet would be synchronised")
	}
	if d := c.backoff(1, true); d < 50*time.Millisecond {
		t.Fatalf("unauthorized backoff: %v", d)
	}
}

func TestExecuteBoundsTheResponse(t *testing.T) {
	big := strings.Repeat("x", MaxResponseBody+1)
	c, _ := NewClient(ClientConfig{HubURL: "https://hub.invalid", Secret: "s", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(big))
	})})
	r := c.Execute(context.Background(), Request{ID: "1", Method: http.MethodGet, Path: "/v1/clubs", RequestID: "rq-1"})
	if r.Status != http.StatusBadGateway || !strings.Contains(string(r.Body), "RESPONSE_TOO_LARGE") || len(r.Body) > 1000 {
		t.Fatalf("%d %.100s", r.Status, r.Body)
	}
	// Authorization is never forwarded into the gateway
	var sawAuth string
	c, _ = NewClient(ClientConfig{HubURL: "https://hub.invalid", Secret: "s", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
	})})
	c.Execute(context.Background(), Request{ID: "1", Method: http.MethodGet, Path: "/v1/clubs", Header: map[string]string{"Authorization": "Bearer partner-key"}})
	if sawAuth != "" {
		t.Fatal("a partner key reached the gateway")
	}
}
