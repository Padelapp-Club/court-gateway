package auth

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

const (
	readKey = "read-key-0123456789abcdef"
	bookKey = "book-key-0123456789abcdef"
)

func setup(t *testing.T, clk *fakeClock) http.Handler {
	t.Helper()
	a, err := New([]Partner{
		{Name: "reader", Key: readKey, Scopes: []Scope{ScopeRead}},
		{Name: "booker", Key: bookKey, Scopes: []Scope{ScopeRead, ScopeBook}, WritesPerMinute: 3},
	}, Options{Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	return a.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, _ := PartnerName(r.Context())
		w.Header().Set("X-Partner", name)
		w.WriteHeader(http.StatusOK)
	}))
}

func do(h http.Handler, method, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/v1/x", nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAuthentication(t *testing.T) {
	h := setup(t, &fakeClock{t: time.Unix(0, 0)})
	cases := []struct {
		name, method, auth string
		want               int
		partner            string
	}{
		{"no header", http.MethodGet, "", 401, ""},
		{"basic scheme", http.MethodGet, "Basic " + readKey, 401, ""},
		{"empty bearer", http.MethodGet, "Bearer ", 401, ""},
		{"unknown key", http.MethodGet, "Bearer nope-nope-nope-nope-nope", 401, ""},
		{"key prefix", http.MethodGet, "Bearer " + readKey[:10], 401, ""},
		{"read key reads", http.MethodGet, "Bearer " + readKey, 200, "reader"},
		{"scheme is case-insensitive", http.MethodGet, "bearer " + readKey, 200, "reader"},
		{"read key cannot post", http.MethodPost, "Bearer " + readKey, 403, ""},
		{"read key cannot delete", http.MethodDelete, "Bearer " + readKey, 403, ""},
		{"book key reads", http.MethodGet, "Bearer " + bookKey, 200, "booker"},
		{"book key posts", http.MethodPost, "Bearer " + bookKey, 200, "booker"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := do(h, c.method, c.auth)
			if rec.Code != c.want || rec.Header().Get("X-Partner") != c.partner {
				t.Fatalf("status %d partner %q; want %d %q; body %s", rec.Code, rec.Header().Get("X-Partner"), c.want, c.partner, rec.Body)
			}
			if c.want == 401 && rec.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("401 without WWW-Authenticate")
			}
		})
	}
}

func TestWriteRateLimit(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	h := setup(t, clk)
	for i := 0; i < 3; i++ {
		if rec := do(h, http.MethodPost, "Bearer "+bookKey); rec.Code != 200 {
			t.Fatalf("write %d refused: %d", i, rec.Code)
		}
	}
	rec := do(h, http.MethodPost, "Bearer "+bookKey)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "20" {
		t.Fatalf("4th write: %d retry-after %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	// reads are not limited
	for i := 0; i < 10; i++ {
		if rec := do(h, http.MethodGet, "Bearer "+bookKey); rec.Code != 200 {
			t.Fatalf("read limited: %d", rec.Code)
		}
	}
	// refill: 3 per minute = one every 20 s
	clk.add(19 * time.Second)
	if rec := do(h, http.MethodDelete, "Bearer "+bookKey); rec.Code != 429 {
		t.Fatalf("refilled too early: %d", rec.Code)
	}
	clk.add(2 * time.Second)
	if rec := do(h, http.MethodDelete, "Bearer "+bookKey); rec.Code != 200 {
		t.Fatalf("not refilled: %d", rec.Code)
	}
}

func TestNewRejectsBadPartners(t *testing.T) {
	bad := [][]Partner{
		nil,
		{{Name: "", Key: "k", Scopes: []Scope{ScopeRead}}},
		{{Name: "a", Key: "k", Scopes: []Scope{"admin"}}},
		{{Name: "a", Key: "k"}, {Name: "a", Key: "j"}},
		{{Name: "a", Key: "k"}, {Name: "b", Key: "k"}},
	}
	for i, ps := range bad {
		if _, err := New(ps, Options{}); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}
