package pair_test

import (
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/pair/pairtest"
)

// R4 (clean-room review of wave 2): a Lunda booking whose length the
// YCLIENTS court's services (30, 60, 90 and 120 minutes on the stub) cannot
// make up is blocked in YCLIENTS by the shortest length they make up that
// covers it — over, never under — in one pass; the next passes leave that
// mirror as it is (no remove and make again), and the source's cancel
// removes it. A length they make up is blocked exactly.
func TestYclientsMirrorCoversLengthServicesCannotMake(t *testing.T) {
	for _, tc := range []struct {
		name          string
		length, cover time.Duration
	}{
		{"45 min blocked for 1 h", 45 * time.Minute, time.Hour},
		{"1 h 15 min blocked for 1 h 30 min", 75 * time.Minute, 90 * time.Minute},
		{"1 h blocked exactly", time.Hour, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := pairtest.YclientsLunda(t, pairtest.Options{At: "10:00"})
			s := env.Day(2, 12, 0)
			id := env.Mirror.Book(t, "court-1", s, s.Add(tc.length))
			for i := range 3 {
				rep := env.Pass(t)
				want := 0
				if i == 0 {
					want = 1
				}
				if rep.Aborted || len(rep.Alarms) > 0 || len(rep.Created) != want || len(rep.Removed) != 0 {
					t.Fatalf("pass %d: created %d removed %d alarms %v, want %d made and nothing removed", i+1, len(rep.Created), len(rep.Removed), rep.Alarms, want)
				}
				m := mirrorsOn(t, env.Primary, "court-1")
				if len(m) != 1 || !m[0].Start.Equal(s) || !m[0].End.Equal(s.Add(tc.cover)) || m[0].Note != "[cg-mirror:lunda:l-"+id+"]" {
					t.Fatalf("pass %d: YCLIENTS mirrors %s, want one over %s", i+1, pairtest.Spans(m), pairtest.Spans([]pairtest.Entry{{Start: s, End: s.Add(tc.cover)}}))
				}
				t.Logf("pass %d: created %d removed %d, YCLIENTS mirror %s", i+1, len(rep.Created), len(rep.Removed), pairtest.Spans(m))
			}
			if m := pairtest.Of(env.Mirror.Live(t), pairtest.Mirror, ""); len(m) != 0 {
				t.Fatalf("the YCLIENTS mirror was mirrored back to Lunda: %s", pairtest.Spans(m))
			}
			env.Mirror.Cancel(t, id)
			if rep := env.Pass(t); len(rep.Removed) != 1 || len(rep.Alarms) > 0 {
				t.Fatalf("pass after the cancel: removed %d alarms %v", len(rep.Removed), rep.Alarms)
			}
			if m := mirrorsOn(t, env.Primary, "court-1"); len(m) != 0 {
				t.Fatalf("the mirror stays after its source's cancel: %s", pairtest.Spans(m))
			}
		})
	}
}
