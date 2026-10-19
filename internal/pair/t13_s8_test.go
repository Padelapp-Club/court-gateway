package pair_test

// T13, scenario 8: no pass ever cancels or changes a club's own booking —
// only the gateway's own mirrors — and the fuse and the incomplete read
// that guard it, on every pair.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda"
	"github.com/Padelapp-Club/court-gateway/internal/pair"
	"github.com/Padelapp-Club/court-gateway/internal/pair/pairtest"
)

// copiedMarker is a mirror's marker as a club's staff could paste it from
// a mirror of crm into a booking of their own there: it names the other
// CRM of the pair, as the mirrors of crm do.
func copiedMarker(env *pairtest.Env, crm pairtest.CRM) string {
	if crm == env.Primary {
		return "[cg-mirror:lunda:l-00000000-0000-4000-8000-0000000000fe]"
	}
	switch env.Primary.Type() {
	case "viva":
		return "[cg-mirror:viva:e-00000000-0000-4000-8000-0000000000ff]"
	case "yclients":
		return "[cg-mirror:yclients:y-990000001]"
	}
	return "[cg-mirror:fitness1c:x-copied-0001]"
}

const lundaCourt3 = "00000000-0000-4000-8000-00000000a1c3"

// Every class of club booking each CRM has — Viva's exercise and break,
// YCLIENTS's record, 1C's rental, Lunda's booking of a club's client and
// of the service client without a marker — stays byte for byte what it
// was through passes with: an incomplete read of either CRM, damaged
// markers, a marker copied into a club's booking (Viva exercise, YCLIENTS
// record of a club's client, Lunda booking of a club's client), two club
// bookings overlapping across the CRMs, more legitimate removals than
// mirror.max_removals, and a court mapping changed so that mirrors fall
// outside it (and back). Every club booking that may be mirrored is (the
// one with a copied marker too); every write of every pass is a mirror's.
func TestT13S8ClubBookingsByteForByte(t *testing.T) {
	eachPair(t, func(t *testing.T, env *pairtest.Env) {
		d := func(day, h int) time.Time { return env.Day(day, h, 0) }
		type want struct {
			dst          pairtest.CRM
			court        string
			start, end   time.Time
			what, handle string
		}
		var mirrored []want
		var pClub, lClub []string
		book := func(crm pairtest.CRM, court string, s, e time.Time, what string, note string, mirror bool) string {
			id := crm.Book(t, court, s, e)
			if note != "" {
				crm.Annotate(t, id, note)
			}
			dst := env.Mirror
			if crm == env.Mirror {
				dst, lClub = env.Primary, append(lClub, id)
			} else {
				pClub = append(pClub, id)
			}
			if mirror {
				mirrored = append(mirrored, want{dst, court, s, e, what, id})
			}
			return id
		}
		book(env.Primary, "court-1", d(6, 8), d(6, 9), "the primary's club booking", "", true)
		copied := env.Primary.Type() != "fitness1c" // a 1C rental has no comment to paste into
		book(env.Primary, "court-1", d(6, 16), d(6, 17), "the primary's booking with a copied marker", copiedMarker(env, env.Primary), true)
		book(env.Mirror, "court-2", d(6, 12), d(6, 13), "Lunda's booking of a club's client", "", true)
		// its mirror in Lunda's court 2 falls outside the mapping below
		book(env.Primary, "court-2", d(6, 13), d(6, 14), "the primary's club booking on court-2", "", true)
		book(env.Mirror, "court-1", d(6, 14), d(6, 15), "Lunda's booking of a club's client with a copied marker", copiedMarker(env, env.Mirror), true)
		// two club bookings of court-2 overlapping across the CRMs: alarmed, neither mirrored
		book(env.Primary, "court-2", d(6, 18), d(6, 19), "", "", false)
		book(env.Mirror, "court-2", d(6, 18), d(6, 20), "", "", false)
		l := env.Mirror.(*pairtest.LundaStub)
		svc := l.BookOnService("court-1", d(6, 10), d(6, 12), "Турнир клуба")
		lClub = append(lClub, svc)
		mirrored = append(mirrored, want{env.Primary, "court-1", d(6, 10), d(6, 12), "Lunda's service-client booking without a marker", svc})
		lClub = append(lClub, l.BookOnService("court-2", d(6, 21), d(6, 22), "[cg-mirror:"+env.Primary.Type()+":e-0000")) // damaged
		if v, ok := env.Primary.(*pairtest.VivaStub); ok {
			br := v.BookBreak("court-2", d(6, 9), d(6, 10), "Ремонт")
			pClub = append(pClub, br)
			mirrored = append(mirrored, want{env.Mirror, "court-2", d(6, 9), d(6, 10), "Viva's break", br})
			pClub = append(pClub, v.BookBreak("court-1", d(6, 20), d(6, 21), "[cg-mirror:lunda:l-x] и ещё текст")) // damaged
		}
		// four Lunda bookings the club will cancel at once: more removals than the fuse's 3
		var four []string
		for i := range 4 {
			s := d(7, 9+2*i)
			four = append(four, book(env.Mirror, "court-1", s, s.Add(time.Hour), "Lunda's booking to cancel", "", true))
		}

		// the club's records as the club left them, before any pass
		ps, ls := snap(t, env.Primary, pClub), snap(t, env.Mirror, lClub)
		same := func(when string) {
			t.Helper()
			ps.same(t, env.Primary, when)
			ls.same(t, env.Mirror, when)
			if t.Failed() {
				t.FailNow()
			}
		}
		rep := env.Settle(t)
		// a club booking left unblocked is an error, not the end: the
		// records are compared all the same
		for _, w := range mirrored {
			if !pairtest.Has(w.dst.Live(t), pairtest.Mirror, w.court, w.start, w.end) {
				t.Errorf("%s (%s) is not blocked in %s: %s", w.what, w.handle, w.dst.Type(), pairtest.Spans(w.dst.Live(t)))
			}
		}
		same("the first passes")
		if !alarmed(rep, pair.AlarmOverlap) || !alarmed(rep, pair.AlarmDamaged) {
			t.Fatalf("first passes: no overlap or damaged alarm: %+v", rep.Alarms)
		}
		if !copied {
			t.Log("1C: no copied marker in the primary (a rental has no comment)")
		}
		checkWrites := func(when string, rep pair.Report) {
			t.Helper()
			for _, it := range append(rep.Created, rep.Removed...) {
				for _, h := range append(append([]string{}, pClub...), lClub...) {
					if strings.Contains(it.ID, h) {
						t.Fatalf("%s: a pass wrote the club's booking %s: %+v", when, h, it)
					}
				}
			}
		}

		for _, side := range []pairtest.CRM{env.Mirror, env.Primary} {
			how, undo := side.Incomplete(t)
			rep := env.Pass(t)
			undo()
			if !rep.Aborted || len(rep.Created)+len(rep.Removed) > 0 || !strings.Contains(alarmText(rep, pair.AlarmIncomplete), incompleteText[how]) {
				t.Fatalf("pass with %s's read %s: %+v", side.Type(), how, rep)
			}
			same(side.Type() + " read " + how)
		}

		for _, id := range four {
			env.Mirror.Cancel(t, id)
		}
		ls = snap(t, env.Mirror, lClub) // the club's own cancels
		for range 2 {
			rep := env.Settle(t)
			checkWrites("fuse", rep)
			if !alarmed(rep, pair.AlarmFuse) || len(rep.Removed) > 0 {
				t.Fatalf("passes after 4 cancels: %+v", rep)
			}
		}
		same("the fuse")

		// court-2's Lunda room changes: Lunda's court 2 is mapped by no court
		l.Stub.AddCourt(lundaCourt3, "Корт 3")
		env.Respec(t, func(s *core.ClubSpec) { s.Courts[1].Params[lunda.CourtKey] = lundaCourt3 })
		for range 2 {
			rep := env.Settle(t)
			checkWrites("mapping changed", rep)
			if !alarmed(rep, pair.AlarmUnmapped) {
				t.Fatalf("the mapping changed, no mirror outside it: %+v", rep)
			}
		}
		same("the mapping changed")
		env.Respec(t, func(s *core.ClubSpec) { s.Courts[1].Params[lunda.CourtKey] = pairtest.LundaCourt2 })
		for range 2 {
			checkWrites("mapping back", env.Settle(t))
		}
		same("the mapping back")

		// the operator raises the fuse: the four mirrors go, nothing of the club's
		four4 := 4
		env.Reconfigure(t, core.MirrorSpec{MaxRemovals: &four4})
		rep = env.Settle(t)
		checkWrites("fuse raised", rep)
		if len(rep.Removed) < 4 {
			t.Fatalf("passes with the fuse raised: %+v", rep)
		}
		for range 2 {
			checkWrites("last", env.Settle(t))
		}
		same("all passes")
	})
}

// The fuse both ways: four club bookings of src cancelled at once against
// a fuse of 3 — no mirror removed, MIRROR_FUSE naming the count and the
// limit, the hub's readiness MIRROR_FUSE with that text; the operator
// raises mirror.max_removals on the hub and the very next pass removes all
// four (1C's markerless mirrors too: the club's memory outlives the pair).
func TestT13FuseBothWays(t *testing.T) {
	eachPairDir(t, func(t *testing.T, env *pairtest.Env, d direction) {
		var ids []string
		for i := range 4 {
			s := env.Day(3, 9+2*i, 0)
			ids = append(ids, d.src.Book(t, "court-1", s, s.Add(time.Hour)))
		}
		if rep := env.Pass(t); len(rep.Created) != 4 {
			t.Fatalf("pass: %+v", rep)
		}
		mirrors := mirrorsOn(t, d.dst, "")
		raw := snap(t, d.dst, handles(mirrors))
		for _, id := range ids {
			d.src.Cancel(t, id)
		}
		rep := d.settle(t, env)
		text := alarmText(rep, pair.AlarmFuse)
		if len(rep.Removed) != 0 || !strings.Contains(text, "к снятию 4 отражений") || !strings.Contains(text, "порог — 3") {
			t.Fatalf("passes after 4 cancels: %+v", rep)
		}
		raw.same(t, d.dst, "the fuse")
		err := env.Built.Clubs.Ready(context.Background())
		if core.ReadyReason(err) != pair.AlarmFuse || !strings.Contains(core.ReadyDetail(err), "к снятию 4 отражений") {
			t.Fatalf("readiness %q, detail %q", core.ReadyReason(err), core.ReadyDetail(err))
		}
		four := 4
		env.Reconfigure(t, core.MirrorSpec{MaxRemovals: &four})
		if rep := env.Pass(t); len(rep.Removed) != 4 || alarmed(rep, pair.AlarmFuse) {
			t.Fatalf("the first pass with the fuse raised: %+v", rep)
		}
		if m := mirrorsOn(t, d.dst, ""); len(m) != 0 {
			t.Fatalf("mirrors left: %s", pairtest.Spans(m))
		}
		if err := env.Built.Clubs.Ready(context.Background()); core.ReadyReason(err) == pair.AlarmFuse {
			t.Fatalf("the fuse's alarm stays: %v", err)
		}
	})
}

// An incomplete read of either CRM (a failed one where the driver has no
// incomplete answer) writes nothing at all — no create, no removal, in
// either CRM — though a create and a removal are due both ways; the next
// pass makes them.
func TestT13IncompleteReadWritesNothing(t *testing.T) {
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			if p.name != "yclients+lunda" {
				t.Parallel()
			}
			for _, side := range []string{"primary", "lunda"} {
				env := p.setup(t, pairtest.Options{})
				t.Run(side, func(t *testing.T) {
					crm := env.Primary
					if side == "lunda" {
						crm = env.Mirror
					}
					d := func(h int) time.Time { return env.Day(3, h, 0) }
					b1 := env.Primary.Book(t, "court-1", d(10), d(11))
					l1 := env.Mirror.Book(t, "court-2", d(12), d(13))
					if rep := env.Pass(t); len(rep.Created) != 2 {
						t.Fatalf("pass: %+v", rep)
					}
					env.Primary.Cancel(t, b1)
					env.Mirror.Cancel(t, l1)
					env.Primary.Book(t, "court-2", d(14), d(15))
					env.Mirror.Book(t, "court-1", d(16), d(17))
					pLive, mLive := env.Primary.Live(t), env.Mirror.Live(t)
					ps, ms := snap(t, env.Primary, handles(pLive)), snap(t, env.Mirror, handles(mLive))
					how, undo := crm.Incomplete(t)
					rep := env.Pass(t)
					undo()
					if !rep.Aborted || !strings.Contains(alarmText(rep, pair.AlarmIncomplete), incompleteText[how]) || len(rep.Created)+len(rep.Removed) > 0 {
						t.Fatalf("pass with the %s read %s: %+v", side, how, rep)
					}
					ps.same(t, env.Primary, "incomplete read")
					ms.same(t, env.Mirror, "incomplete read")
					if !sameEntries(pLive, env.Primary.Live(t)) || !sameEntries(mLive, env.Mirror.Live(t)) {
						t.Fatalf("a pass with the %s read %s wrote:\nprimary %s\nmirror %s", side, how, pairtest.Spans(env.Primary.Live(t)), pairtest.Spans(env.Mirror.Live(t)))
					}
					if r := core.ReadyReason(env.Built.Clubs.Ready(context.Background())); r != pair.AlarmIncomplete {
						t.Fatalf("readiness: %s", r)
					}
					rep = env.Settle(t)
					if rep.Aborted || len(rep.Created) != 2 || len(rep.Removed) != 2 {
						t.Fatalf("the passes after: %+v", rep)
					}
				})
			}
		})
	}
}

// incompleteText tells, in MIRROR_READ_INCOMPLETE, a read the CRM answered
// incompletely from one that failed: each fault takes its own branch.
var incompleteText = map[string]string{"incomplete": "ответила не полностью", "failed": "не прочитана"}
