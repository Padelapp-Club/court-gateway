package pair_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/pair"
	"github.com/Padelapp-Club/court-gateway/internal/pair/pairtest"
)

// The fuse on the stubs: four cancels in Lunda at once against the
// default fuse of 3 — no mirror removed, MIRROR_FUSE; the operator raises
// mirror.max_removals for one pass and they go.
func TestFuseOnStubs(t *testing.T) {
	each(t, func(t *testing.T, env *pairtest.Env) {
		var ids []string
		for i := range 4 {
			s := env.Day(3, 9+2*i, 0)
			ids = append(ids, env.Mirror.Book(t, "court-1", s, s.Add(time.Hour)))
		}
		if rep := env.Pass(t); len(rep.Created) != 4 {
			t.Fatalf("pass: %+v", rep)
		}
		for _, id := range ids {
			env.Mirror.Cancel(t, id)
		}
		rep := env.Pass(t)
		if !alarmed(rep, pair.AlarmFuse) || len(rep.Removed) != 0 {
			t.Fatalf("pass after 4 cancels: %+v", rep)
		}
		if n := len(pairtest.Of(env.Primary.Live(t), pairtest.Mirror, "")); n != 4 {
			t.Fatalf("mirrors left: %d", n)
		}
		if err := env.Built.Driver.Ready(context.Background()); core.ReadyReason(err) != pair.AlarmFuse {
			t.Fatalf("readiness: %v", err)
		}
		four := 4
		env.Reconfigure(t, core.MirrorSpec{MaxRemovals: &four})
		if rep := env.Pass(t); len(rep.Removed) != 4 || alarmed(rep, pair.AlarmFuse) {
			t.Fatalf("pass with the fuse raised: %+v", rep)
		}
		if n := len(pairtest.Of(env.Primary.Live(t), pairtest.Mirror, "")); n != 0 {
			t.Fatalf("mirrors left: %d", n)
		}
	})
}

// A day of Lunda that cannot be read makes its read incomplete: the pass
// writes nothing — not even the creates it could make — and alarms.
func TestIncompleteLundaReadOnStubs(t *testing.T) {
	env := pairtest.VivaLunda(t, pairtest.Options{})
	start := env.Day(2, 10, 0)
	env.Primary.Book(t, "court-1", start, start.Add(time.Hour))
	env.LundaStub.FailNextQuery(http.MethodGet, "/club/booking/slots-load", "date="+env.Day(5, 0, 0).Format("2006-01-02"), http.StatusServiceUnavailable, "{}")
	rep := env.Pass(t)
	if !rep.Aborted || !alarmed(rep, pair.AlarmIncomplete) || len(rep.Created) > 0 {
		t.Fatalf("pass: %+v", rep)
	}
	if len(pairtest.Of(env.Mirror.Live(t), pairtest.Mirror, "")) > 0 {
		t.Fatal("a pass with an incomplete read wrote a mirror")
	}
	if rep := env.Pass(t); rep.Aborted || len(rep.Created) != 1 {
		t.Fatalf("the next pass: %+v", rep)
	}
}

// A failed read of the primary aborts the pass the same way.
func TestFailedPrimaryReadOnStubs(t *testing.T) {
	env := pairtest.VivaLunda(t, pairtest.Options{})
	start := env.Day(2, 10, 0)
	id := env.Mirror.Book(t, "court-1", start, start.Add(time.Hour))
	env.Pass(t)
	env.Mirror.Cancel(t, id)
	v := env.Primary.(*pairtest.VivaStub)
	v.Stub.FailNext(http.MethodGet, "/breaks", http.StatusInternalServerError, "{}")
	rep := env.Pass(t)
	if !rep.Aborted || len(rep.Removed) > 0 {
		t.Fatalf("pass: %+v", rep)
	}
	if len(pairtest.Of(env.Primary.Live(t), pairtest.Mirror, "")) != 1 {
		t.Fatal("a pass with a failed read removed a mirror")
	}
}

// A Lunda booking of a club's client whose notes copy a mirror's marker is
// the club's: never removed (F4).
func TestLundaMarkerOnClubClientIsTheClubs(t *testing.T) {
	env := pairtest.VivaLunda(t, pairtest.Options{})
	start := env.Day(4, 12, 0)
	id := env.Mirror.Book(t, "court-1", start, start.Add(time.Hour))
	env.Mirror.Annotate(t, id, "[cg-mirror:viva:e-00000000-0000-4000-8000-0000000000ff]")
	for range 3 {
		env.Pass(t)
	}
	if b := env.LundaStub.Booking(id); b.Status == "CANCELLED" {
		t.Fatal("the club's booking with a copied marker was cancelled")
	}
	if !pairtest.Has(env.Primary.Live(t), pairtest.Mirror, "court-1", start, start.Add(time.Hour)) {
		t.Fatal("the club's booking with a copied marker is not blocked in the primary")
	}
}

// DeleteBlock removes a block only while it is still the block read: one
// the club has moved (or whose marker it changed) since is left.
func TestDeleteBlockReadsAgain(t *testing.T) {
	env := pairtest.VivaLunda(t, pairtest.Options{})
	s := env.Day(3, 10, 0)
	env.Primary.Book(t, "court-2", s, s.Add(time.Hour))
	src := env.Mirror.Book(t, "court-2", s.Add(2*time.Hour), s.Add(3*time.Hour))
	rep := env.Pass(t)
	if len(rep.Created) != 2 {
		t.Fatalf("pass: %+v", rep)
	}
	for _, it := range rep.Created {
		var m driver.Mirrorer = env.Pair.Mirror().(driver.Mirrorer)
		if it.Source.Src == "lunda" {
			m = env.Pair.Primary().(driver.Mirrorer)
		}
		moved := it
		moved.End = it.End.Add(30 * time.Minute) // not what is there
		if err := m.DeleteBlock(context.Background(), moved); !errors.Is(err, driver.ErrNotOwned) {
			t.Fatalf("%s: a block of another span was deleted: %v", it.ID, err)
		}
		other := it
		other.Source = &driver.MirrorMarker{Src: it.Source.Src, ID: it.Source.ID + "0"}
		if err := m.DeleteBlock(context.Background(), other); !errors.Is(err, driver.ErrNotOwned) {
			t.Fatalf("%s: a block of another source was deleted: %v", it.ID, err)
		}
	}
	if n := len(pairtest.Of(env.Primary.Live(t), pairtest.Mirror, "")) + len(pairtest.Of(env.Mirror.Live(t), pairtest.Mirror, "")); n != 2 {
		t.Fatalf("mirrors left: %d", n)
	}
	_ = src
}
