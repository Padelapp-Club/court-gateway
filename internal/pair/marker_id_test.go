package pair_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver/yclients/yclientsstub"
	"github.com/Padelapp-Club/court-gateway/internal/pair/pairtest"
)

// retag replaces old by new in the comment (notes) of the mirror handle of
// crm, as the stub stores it — keeping anything else there (YCLIENTS's set
// tag).
func retag(t *testing.T, crm pairtest.CRM, handle, old, new string) {
	t.Helper()
	switch c := crm.(type) {
	case *pairtest.YclientsStub:
		id, err := strconv.ParseInt(handle, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		c.Stub.EditRecord(id, func(r *yclientsstub.Record) { r.Comment = strings.Replace(r.Comment, old, new, 1) })
	default:
		for _, e := range crm.Live(t) {
			if e.Handle == handle {
				crm.Annotate(t, handle, strings.Replace(e.Note, old, new, 1))
				return
			}
		}
		t.Fatalf("no entry %s", handle)
	}
}

// A mirror's marker names its source's id whole: a mirror whose marker
// holds a strict prefix of a club booking's id — on that booking's court
// and span — is not that booking's mirror: it is removed (its point read
// finds no such source) and a mirror with the whole id is made. Both ways
// where the target keeps a marker (1C's mirrors have none).
func TestT13MarkerIDIsWhole(t *testing.T) {
	eachPairDir(t, func(t *testing.T, env *pairtest.Env, d direction) {
		if d.markerless {
			t.Skip("1C's mirrors carry no marker: no id to compare")
		}
		s := env.Day(4, 10, 0)
		d.src.Book(t, "court-1", s, s.Add(time.Hour))
		if rep := env.Pass(t); len(rep.Created) != 1 {
			t.Fatalf("pass: %+v", rep)
		}
		m := mirrorsOn(t, d.dst, "court-1")
		if len(m) != 1 || !strings.HasPrefix(m[0].Note, "[cg-mirror:") {
			t.Fatalf("mirror: %s", pairtest.Spans(m))
		}
		whole := strings.TrimSuffix(m[0].Note, "]")
		prefix := whole[:len(whole)-1] // the id less its last character
		retag(t, d.dst, m[0].Handle, whole+"]", prefix+"]")
		if got := mirrorsOn(t, d.dst, "court-1"); len(got) != 1 || got[0].Note != prefix+"]" {
			t.Fatalf("retagged: %s", pairtest.Spans(got))
		}
		rep := env.Pass(t)
		if len(rep.Removed) != 1 || len(rep.Created) != 1 || len(rep.Alarms) > 0 {
			t.Fatalf("pass with a marker of a prefix of the id: %+v", rep)
		}
		if got := mirrorsOn(t, d.dst, "court-1"); len(got) != 1 || got[0].Note != whole+"]" || got[0].Handle == m[0].Handle {
			t.Fatalf("mirrors after: %s", pairtest.Spans(got))
		}
	})
}
