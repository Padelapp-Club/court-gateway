package pair_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/pair"
	"github.com/Padelapp-Club/court-gateway/internal/pair/pairtest"
)

// blockedPairs are the pairs whose primary rounds a Lunda booking out
// (BlockSpan): 1C to whole seances of 30 minutes. Back to back Lunda
// bookings 12:00–12:45 and 12:45–13:45: the first is blocked 12:00–13:00,
// so the block of the second (blocked2) overlaps it and is never made —
// 13:00–13:45 stays open in the primary for good. YCLIENTS blocks the exact
// span and does not belong here (see
// TestYclientsLengthServicesCannotMakeFailsItsCreateOnly).
var blockedPairs = []struct {
	name     string
	setup    pairtest.Setup
	crm      string // the primary as the alarm names it
	blocked2 [2]time.Duration
}{
	{"fitness1c", pairtest.Fitness1CLunda, "1С:Фитнес клуб", [2]time.Duration{30 * time.Minute, 120 * time.Minute}},
}

// backToBack books the two Lunda bookings on court-1 two days ahead.
func backToBack(t *testing.T, env *pairtest.Env) (s time.Time, first, second string) {
	s = env.Day(2, 12, 0)
	first = env.Mirror.Book(t, "court-1", s, s.Add(45*time.Minute))
	second = env.Mirror.Book(t, "court-1", s.Add(45*time.Minute), s.Add(105*time.Minute))
	return s, first, second
}

// onlyFirstBlocked: the primary holds one mirror on court-1, 12:00–13:00.
func onlyFirstBlocked(t *testing.T, env *pairtest.Env, s time.Time, pass int) {
	t.Helper()
	m := mirrorsOn(t, env.Primary, "court-1")
	if len(m) != 1 || !m[0].Start.Equal(s) || !m[0].End.Equal(s.Add(time.Hour)) {
		t.Fatalf("pass %d: primary mirrors %s, want one over %s", pass, pairtest.Spans(m), pairtest.Spans([]pairtest.Entry{{Start: s, End: s.Add(time.Hour)}}))
	}
}

// A needed mirror whose span a valid mirror of another booking holds is
// skipped in every pass; the second complete pass in a row that
// skips it raises MIRROR_OVERLAP naming the court, the time and the CRM —
// not the first. Nothing is made or removed otherwise than without the
// alarm.
func TestMirrorHeldByAnotherMirrorAlarmsOnSecondPass(t *testing.T) {
	for _, pc := range blockedPairs {
		t.Run(pc.name, func(t *testing.T) {
			env := pc.setup(t, pairtest.Options{At: "10:00"})
			s, _, _ := backToBack(t, env)
			b := s.Add(pc.blocked2[0])
			want := pair.Alarm{Code: pair.AlarmOverlap, Problem: fmt.Sprintf(
				"корт court-1, %s–%s: время брони клуба из Лунда не заблокировано в %s — его перекрывает отражение другой брони клуба, два прохода сверки подряд; в %s это время могут продать напрямую, проверьте брони корта в обеих CRM",
				b.In(env.Location).Format("2006-01-02 15:04"), s.Add(pc.blocked2[1]).In(env.Location).Format("15:04"), pc.crm, pc.crm)}
			for i := range 3 {
				rep := env.Pass(t)
				created := 0
				if i == 0 {
					created = 1
				}
				if rep.Aborted || len(rep.Created) != created || len(rep.Removed) != 0 {
					t.Fatalf("pass %d: aborted %v created %d removed %d, want %d made and nothing removed", i+1, rep.Aborted, len(rep.Created), len(rep.Removed), created)
				}
				onlyFirstBlocked(t, env, s, i+1)
				switch {
				case i == 0 && len(rep.Alarms) != 0:
					t.Fatalf("pass 1: alarms %v, want none (one skip is no news)", rep.Alarms)
				case i > 0 && (len(rep.Alarms) != 1 || rep.Alarms[0] != want):
					t.Fatalf("pass %d: alarms %v, want exactly %v", i+1, rep.Alarms, want)
				}
				t.Logf("pass %d: created %d removed %d alarms %v", i+1, len(rep.Created), len(rep.Removed), rep.Alarms)
			}
			if m := pairtest.Of(env.Mirror.Live(t), pairtest.Mirror, ""); len(m) != 0 {
				t.Fatalf("the primary's mirror was mirrored back to Lunda: %s", pairtest.Spans(m))
			}
		})
	}
}

// YCLIENTS blocks a Lunda booking over its exact span. Back to back
// bookings 12:00–12:45 and 12:45–13:45, with services of 30, 60, 90 and
// 120 minutes: the 45-minute one cannot be made and raises
// MIRROR_CREATE_FAILED in every pass; the second is blocked exactly in the
// first pass and stays. Nothing is held by another mirror, so there is no
// MIRROR_OVERLAP.
func TestYclientsLengthServicesCannotMakeFailsItsCreateOnly(t *testing.T) {
	env := pairtest.YclientsLunda(t, pairtest.Options{At: "10:00"})
	s, _, _ := backToBack(t, env)
	for i := range 3 {
		rep := env.Pass(t)
		created := 0
		if i == 0 {
			created = 1
		}
		if rep.Aborted || len(rep.Created) != created || len(rep.Removed) != 0 {
			t.Fatalf("pass %d: aborted %v created %d removed %d, want %d made and nothing removed", i+1, rep.Aborted, len(rep.Created), len(rep.Removed), created)
		}
		if len(rep.Alarms) != 1 || rep.Alarms[0].Code != pair.AlarmCreateFailed || !strings.Contains(rep.Alarms[0].Problem, "12:00–12:45") {
			t.Fatalf("pass %d: alarms %v, want exactly one MIRROR_CREATE_FAILED for 12:00–12:45", i+1, rep.Alarms)
		}
		m := mirrorsOn(t, env.Primary, "court-1")
		if len(m) != 1 || !m[0].Start.Equal(s.Add(45*time.Minute)) || !m[0].End.Equal(s.Add(105*time.Minute)) {
			t.Fatalf("pass %d: YCLIENTS mirrors %s, want the second booking's alone, 12:45–13:45", i+1, pairtest.Spans(m))
		}
		t.Logf("pass %d: created %d removed %d alarms %v", i+1, len(rep.Created), len(rep.Removed), rep.Alarms)
	}
}

// A skip in one pass only raises no alarm: the booking whose mirror held
// the span is cancelled before the next pass; the passes after it remove
// that mirror (once found invalid twice) and block the second booking.
func TestMirrorHeldForOnePassDoesNotAlarm(t *testing.T) {
	for _, pc := range blockedPairs {
		t.Run(pc.name, func(t *testing.T) {
			env := pc.setup(t, pairtest.Options{At: "10:00"})
			s, first, _ := backToBack(t, env)
			if rep := env.Pass(t); rep.Aborted || len(rep.Alarms) != 0 || len(rep.Created) != 1 {
				t.Fatalf("pass 1: aborted %v created %d alarms %v", rep.Aborted, len(rep.Created), rep.Alarms)
			}
			onlyFirstBlocked(t, env, s, 1)
			env.Mirror.Cancel(t, first)
			var made, gone int
			for i := 2; i <= 4; i++ {
				rep := env.Pass(t)
				if rep.Aborted || len(rep.Alarms) != 0 {
					t.Fatalf("pass %d: aborted %v alarms %v, want none", i, rep.Aborted, rep.Alarms)
				}
				made, gone = made+len(rep.Created), gone+len(rep.Removed)
				t.Logf("pass %d: created %d removed %d", i, len(rep.Created), len(rep.Removed))
			}
			m := mirrorsOn(t, env.Primary, "court-1")
			b, e := s.Add(pc.blocked2[0]), s.Add(pc.blocked2[1])
			if made != 1 || gone != 1 || len(m) != 1 || !m[0].Start.Equal(b) || !m[0].End.Equal(e) {
				t.Fatalf("after the cancel: created %d removed %d, primary mirrors %s, want the second booking's alone", made, gone, pairtest.Spans(m))
			}
		})
	}
}

// An aborted pass between two skips breaks "two complete passes in a row":
// no alarm until two complete passes in a row skip again.
func TestMirrorHeldAcrossAnAbortedPassDoesNotAlarm(t *testing.T) {
	for _, pc := range blockedPairs {
		t.Run(pc.name, func(t *testing.T) {
			env := pc.setup(t, pairtest.Options{At: "10:00"})
			backToBack(t, env)
			if rep := env.Pass(t); len(rep.Alarms) != 0 {
				t.Fatalf("pass 1: alarms %v", rep.Alarms)
			}
			_, undo := env.Primary.Incomplete(t)
			if rep := env.Pass(t); !rep.Aborted {
				t.Fatalf("pass 2 not aborted: %+v", rep)
			}
			undo()
			if rep := env.Pass(t); rep.Aborted || len(rep.Alarms) != 0 {
				t.Fatalf("pass 3 (first complete after the abort): aborted %v alarms %v, want none", rep.Aborted, rep.Alarms)
			}
			if rep := env.Pass(t); len(rep.Alarms) != 1 || rep.Alarms[0].Code != pair.AlarmOverlap {
				t.Fatalf("pass 4: alarms %v, want MIRROR_OVERLAP", rep.Alarms)
			}
		})
	}
}

// A hold (a Lunda draft while someone pays) on a needed mirror's span
// stops its create in every pass it is there, and is never
// counted for the alarm: it lasts minutes, longer than a pass.
func TestMirrorHeldByAHoldDoesNotAlarm(t *testing.T) {
	l := newLoop(t)
	s := l.at(1, 10, 0)
	l.a.add(club("e-1", "court-1", s, s.Add(time.Hour)))
	l.b.add(driver.MirrorItem{ID: "ls-draft", Court: "court-1", Start: s, End: s.Add(time.Hour), Class: driver.ClassTransient})
	for i := range 3 {
		if rep := l.pass(); len(rep.Alarms) != 0 || len(l.b.created) != 0 {
			t.Fatalf("pass %d: alarms %v, made %d, want none", i+1, rep.Alarms, len(l.b.created))
		}
	}
}
