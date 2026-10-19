package pair_test

// Regressions of the merge of the two builds of wave 2 (D8–D20 of the
// dual-build comparison, 2026-10-20), on the CRM stubs.

import (
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver/fitness1c/fitness1cstub"
	"github.com/Padelapp-Club/court-gateway/internal/pair/pairtest"
)

// D15: a 1C mirror is told by the mirror client alone. A mirror whose
// service the club edits is still the gateway's: kept while its source
// stands, never taken for the club's busy time (nor mirrored back into
// Lunda), removed once its source is gone.
func TestFitness1CMirrorSurvivesServiceEdit(t *testing.T) {
	env := pairtest.Fitness1CLunda(t, pairtest.Options{})
	f := env.Primary.(*pairtest.Fitness1CStub)
	const other = "00000000-0000-4000-8000-00000000c1e9"
	f.Stub.AddService(fitness1cstub.Service{ID: other, Title: "Аренда корта", Seance: 30, Price: "2000"})
	start := env.Day(3, 10, 0)
	src := env.Mirror.Book(t, "court-1", start, start.Add(time.Hour))
	if rep := env.Pass(t); len(rep.Created) != 1 {
		t.Fatalf("first pass: %+v", rep)
	}
	mirrors := pairtest.Of(env.Primary.Live(t), pairtest.Mirror, "court-1")
	if len(mirrors) != 1 {
		t.Fatalf("1C mirrors: %s", pairtest.Spans(env.Primary.Live(t)))
	}
	m := mirrors[0]
	f.Stub.MoveRental(m.Handle, pairtest.F1CRoom1, other, m.Start, m.End) // the club edits its service
	for i := range 2 {
		if rep := env.Pass(t); len(rep.Created)+len(rep.Removed) > 0 || len(rep.Alarms) > 0 || rep.Aborted {
			t.Fatalf("pass %d after the service edit: %+v", i+1, rep)
		}
	}
	if n := len(pairtest.Of(env.Mirror.Live(t), pairtest.Mirror, "")); n != 0 {
		t.Fatalf("the edited mirror was mirrored back into Lunda: %s", pairtest.Spans(env.Mirror.Live(t)))
	}
	env.Mirror.Cancel(t, src)
	if rep := env.Settle(t); len(rep.Removed) != 1 {
		t.Fatalf("passes after the source's cancel: %+v", rep)
	}
	if n := len(pairtest.Of(env.Primary.Live(t), pairtest.Mirror, "")); n != 0 {
		t.Fatalf("the edited mirror outlived its source: %s", pairtest.Spans(env.Primary.Live(t)))
	}
}
