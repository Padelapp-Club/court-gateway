package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// D12: ReadyDetail takes the Detail texts of the config alerts of a
// readiness failure and nothing else — not an alert's Problems, not an
// upstream error's words — prefixed by the club when several clubs have
// one; control characters dropped, every text and the whole bounded.
func TestReadyDetail(t *testing.T) {
	fuse := &driver.ConfigAlert{Code: "MIRROR_FUSE", Problems: []string{"к снятию 4", "PRIVACY-MARKER-problem"}, Detail: []string{"к снятию 4\n\x07отражений"}}
	zone := &driver.ConfigAlert{Code: "TIMEZONE_MISMATCH", Problems: []string{"PRIVACY-MARKER-zone"}}
	overlap := &driver.ConfigAlert{Code: "MIRROR_OVERLAP", Detail: []string{"брони пересекаются"}}

	c := clubsOf(t, slowDriver{err: fuse}, slowDriver{err: zone}, slowDriver{err: overlap})
	err := c.Ready(context.Background())
	if got, want := ReadyDetail(err), "клуб a: к снятию 4отражений; клуб c: брони пересекаются"; got != want {
		t.Fatalf("three clubs: %q, want %q", got, want)
	}
	c = clubsOf(t, slowDriver{err: fuse})
	if got := ReadyDetail(c.Ready(context.Background())); got != "к снятию 4отражений" {
		t.Fatalf("one club: %q", got)
	}
	for _, e := range []error{nil, zone, errors.New("PRIVACY-MARKER upstream said"), driver.Upstream(driver.UpstreamInvalid, "PRIVACY-MARKER body"),
		&driver.MirrorError{Err: errors.New("PRIVACY-MARKER")}} {
		if got := ReadyDetail(e); got != "" {
			t.Fatalf("%v: %q", e, got)
		}
	}
	long := &driver.ConfigAlert{Code: "MIRROR_BACKLOG"}
	for range 10 {
		long.Detail = append(long.Detail, strings.Repeat("ж", 700))
	}
	got := ReadyDetail(long)
	if n := utf8.RuneCountInString(got); n > readyDetailMax || !utf8.ValidString(got) {
		t.Fatalf("bounded: %d runes, valid %v", n, utf8.ValidString(got))
	}
	if strings.Contains(got, strings.Repeat("ж", readyDetailText+1)) {
		t.Fatal("a text over its own bound")
	}
}
