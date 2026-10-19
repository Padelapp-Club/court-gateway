package pair

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// The reconciliation loop (REQ-398 в–е). A pass reads both CRMs whole over
// its window, plans everything, then writes:
//
//   - a club booking of one CRM (an item of class club) with no valid
//     mirror in the other gets one: a block of the other CRM's own kind
//     with the marker "[cg-mirror:<src>:<id>]" (or, in 1C, on the mirror
//     client);
//   - a mirror is valid only when the CRM of its source has a club item
//     of the id in its marker (where there is no marker, any club item)
//     on the same court, with the span the mirror takes for it — the unit
//     of mirroring is one interval of one court (MIRROR VALIDITY RULE);
//   - a mirror that is not valid is removed, after a point read of its
//     source by id says the source is gone, cancelled or moved (where no
//     point read exists: invalid in two passes in a row);
//   - nothing else is ever written: a partner's booking (class own) is in
//     both CRMs already, a mirror is never mirrored, a club's booking is
//     never changed or cancelled;
//   - the fuse: a read that fails or may be incomplete aborts the pass
//     before any write; more removals than MaxRemovals in the plan: none is
//     made. Both raise an alarm;
//   - two club bookings of the court that overlap across the CRMs, a
//     partner's booking in one CRM only, a damaged marker: alarms, nothing
//     touched; so is a mirror off the court mapping, or one naming a CRM
//     that is not the pair's other one (MIRROR_UNMAPPED).
//
// Which mirror belongs to which booking is read again every pass from the
// CRMs; the pass remembers in memory only what the two-pass rules need
// (memo, in the club's Memory: kept across a new config of the club, lost
// with the process).

// The window of a pass (D8) starts at now, the hub's signed time: a club
// booking under way or about to start is mirrored as well as a later one
// (a booking that ends past the horizon is left for a later pass, when it
// is within it).
//
// Lead: the CRMs are read from this far ahead of now — the range a read is
// complete for (near midnight a CRM's "today" may already be the next day
// for its clock, and Viva leaves out its past days). Every driver lists
// what overlaps the range read, so a booking under way or starting within
// the lead is mirrored as the read lists it. A mirror is removed when it
// lies within the range read whole — or when it starts within the lead but
// has not started, and every source it could be valid for overlaps that
// range (removable): the same evidence then holds for it (T13-F1, a club
// booking cancelled minutes before its start frees its slot).
const Lead = 5 * time.Minute

// BlockGrid: no block is written to start in the past (the drivers' own
// rules hold a started block for good: it cannot be rolled back or
// removed, and [UNVERIFIED] whether a CRM accepts one at all). A booking
// whose block would start before now — one under way, or one 1C rounds
// out to a seance already begun — is blocked from the first step of this
// grid of the club's day after now; up to one step of it stays open.
const BlockGrid = 30 * time.Minute

// Alarm codes, by rank (the first is the readiness code when several are
// raised).
const (
	AlarmFuse         = "MIRROR_FUSE"
	AlarmHalfBooking  = "MIRROR_HALF_BOOKING"
	AlarmOverlap      = "MIRROR_OVERLAP"
	AlarmDamaged      = "MIRROR_DAMAGED"
	AlarmUnmapped     = "MIRROR_UNMAPPED"
	AlarmRemoveFailed = "MIRROR_REMOVE_FAILED"
	AlarmCreateFailed = "MIRROR_CREATE_FAILED"
	AlarmBacklog      = "MIRROR_BACKLOG"
	AlarmIncomplete   = "MIRROR_READ_INCOMPLETE"
)

var alarmRank = []string{AlarmFuse, AlarmHalfBooking, AlarmOverlap, AlarmDamaged, AlarmUnmapped, AlarmRemoveFailed, AlarmCreateFailed, AlarmBacklog, AlarmIncomplete}

// Alarm is something the operator and the club must look at. Problem is
// in Russian and carries no personal data: courts, times, counts, ids.
type Alarm struct {
	Code    string
	Problem string
}

// stickyPasses is how many passes an alarm raised outside a pass (a
// failed rollback of a partner's booking) stays: until the pass's own
// half-booking rule has seen the booking twice.
const stickyPasses = 3

type sticky struct {
	Alarm
	left int
}

// backlogPasses: creates cut off by MaxCreates in so many passes in a row
// raise an alarm.
const backlogPasses = 3

// memo is what one pass leaves the next, in memory only: keys seen once.
type memo struct {
	suspect map[string]bool // mirrors invalid without a point read
	half    map[string]bool // partners' bookings found in one CRM only
	capped  int             // passes in a row cut off by MaxCreates
	// mirrors to make whose span a mirror the pass does not remove held
	blocked map[string]bool
}

// Report is what a pass did.
type Report struct {
	// Skipped: the hub's time is not known yet; nothing was read.
	Skipped bool
	// Aborted: a read failed or may be incomplete; nothing was written.
	Aborted bool
	Created []driver.MirrorItem
	Removed []driver.MirrorItem
	Alarms  []Alarm
	// Recheck: the pass found a mirror invalid that only a second pass in
	// a row may remove (no marker, no point read of its source) and had not
	// found so before: the sooner the next pass, the sooner it goes (the
	// scheduler runs one Recheck later, D11).
	Recheck bool
}

// side is one CRM in a pass.
type side struct {
	typ  string
	m    driver.Mirrorer
	inv  driver.Inventory
	name string // as the club knows it
}

// unit is one interval of a club item to mirror (an opaque item cut to
// the club's hours may be several).
type unit struct {
	driver.MirrorItem
}

type createPlan struct {
	src, dst *side
	u        unit
	start    time.Time // the block's span in dst
	end      time.Time
}

type removalPlan struct {
	src, dst *side // the CRM of the source, the CRM of the mirror
	it       driver.MirrorItem
	key      string
}

var markerIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

// Reconcile runs one pass of the loop. Its alarms replace the previous
// pass's in Ready (an aborted pass's too). It never returns an error: what
// went wrong is in the report's alarms and the log.
func (p *Pair) Reconcile(ctx context.Context) Report {
	p.pass.Lock()
	defer p.pass.Unlock()
	var rep Report
	now, known := p.o.Clock()
	if !known {
		rep.Skipped = true
		p.forget()
		return rep
	}
	A := &side{typ: p.o.PrimaryType, m: p.am, name: crmName(p.o.PrimaryType)}
	B := &side{typ: p.o.MirrorType, m: p.bm, name: crmName(p.o.MirrorType)}
	// the window ends at the shorter horizon of the two CRMs (D9: Lunda's
	// is its club's booking depth, read now)
	var ha, hb time.Duration
	var ea, eb error
	both(func() { ha, ea = A.m.MirrorHorizon(ctx) }, func() { hb, eb = B.m.MirrorHorizon(ctx) })
	from, to := now.Add(Lead), now.Add(min(ha, hb))
	if ea == nil && eb == nil {
		q := driver.OccupancyQuery{Courts: p.o.Courts, From: from, To: to}
		both(func() { A.inv, ea = A.m.MirrorInventory(ctx, q) }, func() { B.inv, eb = B.m.MirrorInventory(ctx, q) })
	}
	for _, s := range []struct {
		sd  *side
		err error
	}{{A, ea}, {B, eb}} {
		switch {
		case s.err != nil:
			rep.Aborted = true
			p.log.Warn("reconciliation: a CRM could not be read: the pass is aborted, nothing is written", "crm", s.sd.typ, "error", s.err)
			rep.Alarms = append(rep.Alarms, Alarm{AlarmIncomplete, fmt.Sprintf("сверка CRM пропущена: %s не прочитана (%v) — ничего не создано и не снято", s.sd.name, errText(s.err))})
		case !s.sd.inv.Complete:
			rep.Aborted = true
			p.log.Warn("reconciliation: a CRM's read may be incomplete: the pass is aborted, nothing is written", "crm", s.sd.typ)
			rep.Alarms = append(rep.Alarms, Alarm{AlarmIncomplete, fmt.Sprintf("сверка CRM пропущена: %s ответила не полностью — ничего не создано и не снято", s.sd.name)})
		}
	}
	if rep.Aborted {
		// a pass that did not read both CRMs whole breaks "two complete
		// passes in a row" (F11, D16): what the passes before saw is
		// forgotten, the count starts again from the next complete pass
		p.finish(&rep, &memo{})
		return rep
	}
	pl := p.plan(A, B, now, from, to)
	rep.Alarms = append(rep.Alarms, pl.alarms...)
	p.execute(ctx, &pl, &rep)
	p.finish(&rep, &pl.next)
	return rep
}

// errText is an error for an alarm: its kind, not its details.
func errText(err error) string {
	var ue *driver.UpstreamError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "нет ответа"
	case errors.As(err, &ue) && ue.Kind == driver.UpstreamAuth:
		return "нет входа в CRM"
	case errors.As(err, &ue) && ue.Kind == driver.UpstreamInvalid:
		return "непонятный ответ CRM"
	}
	return "CRM недоступна"
}

// finish records the pass's alarms (deduplicated, sticky ones added) and
// what the next pass must remember (nothing after an aborted pass: the
// evidence of passes in a row is broken).
func (p *Pair) finish(rep *Report, next *memo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range next.suspect {
		if !p.memo.suspect[k] {
			rep.Recheck = true
		}
	}
	p.memo = *next
	var keep []sticky
	for _, s := range p.sticky {
		rep.Alarms = append(rep.Alarms, s.Alarm)
		if s.left--; s.left > 0 {
			keep = append(keep, s)
		}
	}
	p.sticky = keep
	seen := map[Alarm]bool{}
	var out []Alarm
	for _, a := range rep.Alarms {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return slices.Index(alarmRank, out[i].Code) < slices.Index(alarmRank, out[j].Code)
	})
	rep.Alarms = out
	p.alarms = out
	for _, a := range out {
		p.log.Error("reconciliation alarm", "code", a.Code, "problem", a.Problem)
	}
}

// forget drops what the passes before saw: a pass that read nothing (the
// hub's time not known) breaks passes in a row as an aborted one does.
func (p *Pair) forget() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.memo = memo{}
}

// stick raises an alarm outside a pass.
func (p *Pair) stick(a Alarm) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sticky = append(p.sticky, sticky{Alarm: a, left: stickyPasses})
	p.alarms = append(p.alarms, a)
}

// Alarms are the last pass's alarms and those raised since.
func (p *Pair) Alarms() []Alarm {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.alarms)
}

// alarmAlert is the alarms as a readiness alert (nil without one).
func (p *Pair) alarmAlert() *driver.ConfigAlert {
	al := p.Alarms()
	if len(al) == 0 {
		return nil
	}
	out := &driver.ConfigAlert{Code: al[0].Code}
	for _, a := range al {
		out.Problems = append(out.Problems, a.Problem)
	}
	// the alarms' texts are the loop's own templates (courts, times,
	// counts, ids; errText for a failure): the hub shows them (D12)
	out.Detail = slices.Clone(out.Problems)
	return out
}

type plan struct {
	creates  []createPlan
	removals []removalPlan
	fused    bool
	alarms   []Alarm
	next     memo
	// prevSuspect: mirrors the previous pass found invalid without a point read
	prevSuspect map[string]bool
	// prevBlocked: mirrors to make the previous pass found held (blockedBy)
	prevBlocked map[string]bool
	// now is the pass's time, cut the step of BlockGrid a block of a
	// booking under way starts at
	now, cut time.Time
}

// gridAfter is the first step of BlockGrid of t's day (in loc) after t.
func gridAfter(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	day := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
	step := day.Add(l.Sub(day).Truncate(BlockGrid))
	for !step.After(t) {
		step = step.Add(BlockGrid)
	}
	return step
}

// blockOf is the span a block of u takes in dst at the pass's time: the
// span dst gives u while it starts after now; once that start has passed
// (u under way, or rounded out to a seance begun), the rest of it from cut
// on — ok is false when nothing of it is left after cut.
func blockOf(u unit, dst driver.Mirrorer, now, cut time.Time) (start, end time.Time, ok bool) {
	start, end = dst.BlockSpan(u.Start, u.End)
	if start.After(now) {
		return start, end, true
	}
	if !cut.Before(u.End) {
		return start, end, false
	}
	start, end = dst.BlockSpan(cut, u.End)
	return start, end, start.After(now) && end.After(start)
}

// inWindow: the interval is wholly within [from, to].
func inWindow(start, end, from, to time.Time) bool { return !start.Before(from) && !end.After(to) }

// removable: an invalid mirror m in dst may be judged by the pass of time
// now that read both CRMs whole over [from, to] — it has not started, ends
// within the horizon, and every source it could be valid for overlaps
// [from, to), so a source still there was listed by the read (or, with a
// marker, is found by the point read remove makes before any removal).
// That holds for a mirror wholly within [from, to]; for one that starts
// within the lead (T13-F1) only while it ends after the last end a source
// ending by from could give it (dst rounds a block out: BlockSpan).
func removable(m driver.MirrorItem, dst driver.Mirrorer, now, from, to time.Time) bool {
	switch {
	case m.Started || !m.Start.After(now) || m.End.After(to):
		return false
	case !m.Start.Before(from):
		return true
	}
	_, last := dst.BlockSpan(now, from)
	return m.End.After(last)
}

func overlaps(a, b driver.MirrorItem) bool {
	return a.Court == b.Court && a.Start.Before(b.End) && b.Start.Before(a.End)
}

// units are the club items of s to mirror within the window [now, to),
// opaque ones cut to the club's hours: every one that has not ended by
// now — under way or about to start too (D8) — and ends within the
// horizon.
func (p *Pair) units(items []driver.MirrorItem, now, to time.Time) []unit {
	var out []unit
	for _, it := range items {
		if it.Class != driver.ClassClub || !slices.Contains(p.o.Courts, it.Court) {
			continue
		}
		for _, piece := range p.clip(it) {
			if piece.End.After(now) && !piece.End.After(to) {
				out = append(out, unit{piece})
			}
		}
	}
	return out
}

// clip cuts an opaque item to the club's hours (one piece per day it is
// open); any other item is itself.
func (p *Pair) clip(it driver.MirrorItem) []driver.MirrorItem {
	if !it.Opaque {
		return []driver.MirrorItem{it}
	}
	var out []driver.MirrorItem
	loc := p.o.Location
	s := it.Start.In(loc)
	for d := time.Date(s.Year(), s.Month(), s.Day(), 0, 0, 0, 0, loc); d.Before(it.End); d = d.AddDate(0, 0, 1) {
		h := p.o.Hours[(int(d.Weekday())+6)%7]
		if h == nil {
			continue
		}
		open := time.Date(d.Year(), d.Month(), d.Day(), 0, h[0], 0, 0, loc)
		closing := time.Date(d.Year(), d.Month(), d.Day(), 0, h[1], 0, 0, loc)
		piece := it
		if open.After(piece.Start) {
			piece.Start = open
		}
		if closing.Before(piece.End) {
			piece.End = closing
		}
		if piece.End.After(piece.Start) {
			out = append(out, piece)
		}
	}
	return out
}

// valid: mirror m (in dst) mirrors u (a club unit of src): the same court,
// the id of its marker (unless it has none), the span a block of u takes —
// or, once that span has begun by now, its rest from a step of the grid
// no later than cut (the block made for u under way, blockOf).
func valid(m driver.MirrorItem, u unit, srcType string, dst driver.Mirrorer, now, cut time.Time) bool {
	if m.Court != u.Court {
		return false
	}
	if m.Source != nil && (m.Source.Src != srcType || m.Source.ID != u.ID) {
		return false
	}
	bs, be := dst.BlockSpan(u.Start, u.End)
	if m.Start.Equal(bs) && m.End.Equal(be) {
		return true
	}
	return !bs.After(now) && m.End.Equal(be) && m.Start.After(bs) && !m.Start.After(cut)
}

func itemKey(it driver.MirrorItem) string {
	return it.ID + "|" + it.Court + "|" + it.Start.UTC().Format(time.RFC3339) + "|" + it.End.UTC().Format(time.RFC3339)
}

// plan reads the two inventories and decides the pass: now is its time,
// [from, to) the range both CRMs were read whole over.
func (p *Pair) plan(A, B *side, now, from, to time.Time) plan {
	pl := plan{next: memo{suspect: map[string]bool{}, half: map[string]bool{}, blocked: map[string]bool{}}, now: now, cut: gridAfter(now, p.o.Location)}
	p.mu.Lock()
	prev := p.memo
	p.mu.Unlock()
	alarm := func(code, format string, a ...any) {
		pl.alarms = append(pl.alarms, Alarm{code, fmt.Sprintf(format, a...)})
	}

	unitsOf := map[*side][]unit{A: p.units(A.inv.Items, now, to), B: p.units(B.inv.Items, now, to)}

	// two club bookings of one court across the CRMs: nothing is touched
	for _, ua := range unitsOf[A] {
		for _, ub := range unitsOf[B] {
			if overlaps(ua.MirrorItem, ub.MirrorItem) {
				alarm(AlarmOverlap, "корт %s: брони клуба пересекаются — %s в %s и %s в %s; шлюз их не трогает, разберитесь в CRM",
					ua.Court, p.span(ua.Start, ua.End), A.name, p.span(ub.Start, ub.End), B.name)
			}
		}
	}

	// damaged markers: untouched, reported
	for _, s := range []*side{A, B} {
		for _, it := range s.inv.Items {
			if it.Class == driver.ClassDamaged && it.End.After(from) && it.Start.Before(to) {
				alarm(AlarmDamaged, "корт %s, %s: в %s запись с повреждённой меткой отражения (%s) — шлюз её не трогает и не отражает; исправьте или удалите её в CRM",
					it.Court, p.span(it.Start, it.End), s.name, it.ID)
			}
		}
	}

	// mirrors on rooms no court of the club maps: untouched, reported (F6)
	for _, s := range []*side{A, B} {
		for _, it := range s.inv.Unmapped {
			if it.End.After(now) && it.Start.Before(to) {
				alarm(AlarmUnmapped, "%s: отражение %s (%s) стоит на корте %s, которого нет в сопоставлении кортов клуба — шлюз его не трогает; верните корт в сопоставление или снимите отражение в CRM вручную",
					s.name, it.ID, p.span(it.Start, it.End), it.Court)
			}
		}
	}

	// a partner's booking in one CRM only: reported once it is seen twice
	type ownKey struct {
		court, partner string
		start, end     int64
	}
	owns := func(s *side) map[ownKey]driver.MirrorItem {
		out := map[ownKey]driver.MirrorItem{}
		for _, it := range s.inv.Items {
			if it.Class == driver.ClassOwn && inWindow(it.Start, it.End, from, to) {
				out[ownKey{it.Court, it.Partner, it.Start.UnixNano(), it.End.UnixNano()}] = it
			}
		}
		return out
	}
	oa, ob := owns(A), owns(B)
	for _, d := range []struct {
		have, other map[ownKey]driver.MirrorItem
		in, notIn   *side
	}{{oa, ob, A, B}, {ob, oa, B, A}} {
		for k, it := range d.have {
			if _, ok := d.other[k]; ok {
				continue
			}
			key := d.in.typ + "|" + itemKey(it)
			pl.next.half[key] = true
			if prev.half[key] {
				alarm(AlarmHalfBooking, "корт %s, %s: бронь партнёра %s (%s) есть в %s, а в %s её нет — шлюз ничего не отменяет и не досоздаёт; выясните, какая половина верна, и исправьте вручную",
					it.Court, p.span(it.Start, it.End), it.Partner, it.ID, d.in.name, d.notIn.name)
			}
		}
	}

	// mirrors and the club bookings they are for, both ways
	for _, dir := range [][2]*side{{A, B}, {B, A}} {
		src, dst := dir[0], dir[1]
		units := unitsOf[src]
		matched := make([]bool, len(units))
		var invalid []driver.MirrorItem
		for _, m := range dst.inv.Items {
			if m.Class != driver.ClassMirror || !slices.Contains(p.o.Courts, m.Court) {
				continue
			}
			// a mirror whose marker names a CRM that is not the other one
			// of this pair is not ours to judge: untouched, reported (F6)
			if m.Source != nil && m.Source.Src != src.typ {
				if m.End.After(now) {
					alarm(AlarmUnmapped, "корт %s, %s: в %s отражение %s с меткой CRM «%s», а вторая CRM клуба — %s; шлюз его не трогает — проверьте, откуда оно, и снимите его в CRM вручную",
						m.Court, p.span(m.Start, m.End), dst.name, m.ID, m.Source.Src, src.name)
				}
				continue
			}
			found := false
			for i, u := range units {
				if !matched[i] && valid(m, u, src.typ, dst.m, pl.now, pl.cut) {
					matched[i], found = true, true
					break
				}
			}
			if found || !removable(m, dst.m, now, from, to) {
				continue // valid, or started, or its source may lie beyond the range read: left alone
			}
			invalid = append(invalid, m)
		}
		for _, m := range invalid {
			r := removalPlan{src: src, dst: dst, it: m, key: dst.typ + "|" + itemKey(m)}
			if m.Source == nil {
				// no marker, no point read of its source: invalid in two
				// complete passes in a row
				pl.next.suspect[r.key] = true
				if !prev.suspect[r.key] {
					continue
				}
			}
			pl.removals = append(pl.removals, r)
		}
		for i, u := range units {
			if matched[i] {
				continue
			}
			if !markerIDRe.MatchString(u.ID) {
				alarm(AlarmCreateFailed, "корт %s, %s: бронь клуба в %s с идентификатором, который не помещается в метку отражения — в %s она не заблокирована",
					u.Court, p.span(u.Start, u.End), src.name, dst.name)
				continue
			}
			bs, be, ok := blockOf(u, dst.m, pl.now, pl.cut)
			if !ok {
				continue // under way, and over before the next step of the grid
			}
			pl.creates = append(pl.creates, createPlan{src: src, dst: dst, u: u, start: bs, end: be})
		}
	}

	// the fuse: more removals than allowed — none at all
	if n := len(pl.removals); n > p.o.MaxRemovals {
		pl.fused = true
		p.log.Error("reconciliation fuse: too many mirrors to remove in one pass, none is removed", "planned", n, "limit", p.o.MaxRemovals)
		// the way out (F7, D10): not "for one pass" — a markerless mirror
		// (1C) is removed by the second pass that finds it invalid
		alarm(AlarmFuse, "сверка CRM: к снятию %d отражений за один проход, а порог — %d; ничего не снято. Проверьте отмены в CRM; если они верны, поднимите mirror.max_removals клуба на хабе до %d или выше, держите порог поднятым, пока тревога не уйдёт, затем верните",
			n, p.o.MaxRemovals, n)
		pl.removals = nil
	}

	// creates: the earliest first, at most MaxCreates a pass
	sort.SliceStable(pl.creates, func(i, j int) bool { return pl.creates[i].start.Before(pl.creates[j].start) })
	pl.next.capped = 0
	if len(pl.creates) > p.o.MaxCreates {
		pl.next.capped = prev.capped + 1
		p.log.Warn("reconciliation: more mirrors to make than one pass makes, the rest wait", "planned", len(pl.creates), "limit", p.o.MaxCreates)
		if pl.next.capped >= backlogPasses {
			alarm(AlarmBacklog, "сверка CRM: %d отражений ждут создания, а за проход создаётся не больше %d — уже %d проходов подряд; если броней действительно столько, поднимите mirror.max_creates клуба",
				len(pl.creates), p.o.MaxCreates, pl.next.capped)
		}
		pl.creates = pl.creates[:p.o.MaxCreates]
	}
	pl.prevSuspect = prev.suspect
	pl.prevBlocked = prev.blocked
	return pl
}

// execute makes the plan's writes, each under the lock of its court in
// both CRMs.
func (p *Pair) execute(ctx context.Context, pl *plan, rep *Report) {
	pending := map[string]*removalPlan{}
	for i := range pl.removals {
		pending[pl.removals[i].key] = &pl.removals[i]
	}
	done := map[string]bool{}
	for _, c := range pl.creates {
		if ctx.Err() != nil {
			return
		}
		p.create(ctx, c, pending, done, pl, rep)
	}
	for _, r := range pl.removals {
		if ctx.Err() != nil {
			return
		}
		if done[r.key] {
			continue
		}
		unlock, err := p.lock(ctx, r.it.Court)
		if err != nil {
			p.log.Info("reconciliation: the court is busy, its mirror is removed by a later pass", "court", r.it.Court)
			continue
		}
		p.remove(ctx, r, pl, rep)
		unlock()
	}
}

// create makes one mirror. Under the court's lock the target CRM is read
// again over the block's span: anything there but a mirror the pass
// removes anyway means it is not made (an alarm, unless it is a hold of a
// few minutes, or a mirror: then in the second pass in a row, blockedBy);
// such mirrors are removed first (a moved booking whose old mirror is in
// the way).
func (p *Pair) create(ctx context.Context, c createPlan, pending map[string]*removalPlan, done map[string]bool, pl *plan, rep *Report) {
	unlock, err := p.lock(ctx, c.u.Court)
	if err != nil {
		p.log.Info("reconciliation: the court is busy, its mirror is made by a later pass", "court", c.u.Court)
		return
	}
	defer unlock()
	now := driver.MirrorItem{Court: c.u.Court, Start: c.start, End: c.end}
	inv, err := c.dst.m.MirrorInventory(ctx, driver.OccupancyQuery{Courts: []string{c.u.Court}, From: c.start, To: c.end})
	if err != nil || !inv.Complete {
		p.log.Warn("reconciliation: the court could not be read again before a mirror is made; a later pass makes it", "court", c.u.Court, "crm", c.dst.typ, "error", err)
		return
	}
	var first []*removalPlan
	for _, it := range inv.Items {
		if !overlaps(it, now) {
			continue
		}
		key := c.dst.typ + "|" + itemKey(it)
		switch r := pending[key]; {
		case it.Class == driver.ClassMirror && r != nil && !pl.fused:
			first = append(first, r)
		case it.Class == driver.ClassMirror, it.Class == driver.ClassTransient:
			// a mirror not removed yet (it may be valid until the next
			// pass reads its source) or a hold: the next pass sees again
			if it.Class == driver.ClassMirror && r == nil && !pl.next.suspect[key] {
				p.blockedBy(c, pl, rep) // a mirror the pass keeps
			}
			return
		default:
			rep.Alarms = append(rep.Alarms, Alarm{AlarmOverlap, fmt.Sprintf(
				"корт %s: бронь клуба %s в %s не заблокирована в %s — там уже занято (%s)",
				c.u.Court, p.span(c.u.Start, c.u.End), c.src.name, c.dst.name, p.span(it.Start, it.End))})
			return
		}
	}
	for _, r := range first {
		if !p.remove(ctx, *r, pl, rep) {
			return
		}
		done[r.key] = true
	}
	// the block's span as planned (blockOf: from the grid's step for a
	// booking under way); BlockSpan of it is itself
	req := driver.BlockRequest{Court: c.u.Court, Start: c.start, End: c.end, Source: driver.MirrorMarker{Src: c.src.typ, ID: c.u.ID}}
	it, err := c.dst.m.CreateBlock(ctx, req)
	if err != nil {
		p.log.Error("reconciliation: a mirror could not be made", "court", c.u.Court, "crm", c.dst.typ, "source", c.u.ID, "error", err)
		rep.Alarms = append(rep.Alarms, Alarm{AlarmCreateFailed, fmt.Sprintf(
			"корт %s, %s: бронь клуба в %s не удалось заблокировать в %s (%s) — сверка повторит",
			c.u.Court, p.span(c.u.Start, c.u.End), c.src.name, c.dst.name, errText(err))})
		return
	}
	p.log.Info("reconciliation: mirror made", "court", c.u.Court, "crm", c.dst.typ, "mirror", it.ID, "source", c.u.ID,
		"start", c.u.Start.UTC().Format(time.RFC3339), "end", c.u.End.UTC().Format(time.RFC3339))
	rep.Created = append(rep.Created, it)
}

// blockedBy: the span of mirror c is held by a mirror the pass neither
// removes nor suspects (one invalid without a point read goes by the next
// pass). Once is no news: that mirror's source may be cancelled or moved
// by the next pass. The same unit held so in two complete passes in a row
// may be held for good — a valid mirror of another booking that BlockSpan
// rounded out over it — and its time stays open in dst: an alarm. A hold
// (transient, minutes) is not counted; an aborted pass forgets the count
// (memo).
func (p *Pair) blockedBy(c createPlan, pl *plan, rep *Report) {
	key := c.src.typ + ">" + c.dst.typ + "|" + itemKey(c.u.MirrorItem)
	pl.next.blocked[key] = true
	if !pl.prevBlocked[key] {
		return
	}
	p.log.Error("reconciliation: a mirror cannot be made, another mirror holds its span", "court", c.u.Court, "crm", c.dst.typ, "source", c.u.ID)
	rep.Alarms = append(rep.Alarms, Alarm{AlarmOverlap, fmt.Sprintf(
		"корт %s, %s: время брони клуба из %s не заблокировано в %s — его перекрывает отражение другой брони клуба, два прохода сверки подряд; в %s это время могут продать напрямую, проверьте брони корта в обеих CRM",
		c.u.Court, p.span(c.start, c.end), c.src.name, c.dst.name, c.dst.name)})
}

// remove removes one mirror, the caller holding its court's lock, once a
// point read of its source by id says the source is gone (where the CRM
// has none, the plan has seen the mirror invalid twice). The driver reads
// the mirror again and removes it only if it is still that mirror. It
// reports whether the mirror is gone.
func (p *Pair) remove(ctx context.Context, r removalPlan, pl *plan, rep *Report) bool {
	if r.it.Source != nil {
		items, err := r.src.m.MirrorLookup(ctx, r.it.Source.ID)
		switch {
		case errors.Is(err, driver.ErrNotSupported):
			// no point read: invalid in two passes in a row, as without a marker
			pl.next.suspect[r.key] = true
			if !pl.prevSuspect[r.key] {
				return false
			}
		case err != nil:
			p.log.Warn("reconciliation: the source of a mirror could not be read again; it is not removed", "mirror", r.it.ID, "source", r.it.Source.ID, "error", err)
			return false
		default:
			for _, it := range items {
				for _, piece := range p.clip(it) {
					if piece.Class == driver.ClassClub && valid(r.it, unit{piece}, r.src.typ, r.dst.m, pl.now, pl.cut) {
						p.log.Info("reconciliation: the source of a mirror is still there; it is kept", "mirror", r.it.ID, "source", r.it.Source.ID)
						return false
					}
				}
			}
		}
	}
	err := r.dst.m.DeleteBlock(ctx, r.it)
	switch {
	case errors.Is(err, driver.ErrNotOwned), errors.Is(err, driver.ErrStarted):
		p.log.Info("reconciliation: a mirror changed since it was read; it is not removed", "mirror", r.it.ID, "error", err)
		return false
	case err != nil:
		p.log.Error("reconciliation: a mirror could not be removed", "mirror", r.it.ID, "crm", r.dst.typ, "error", err)
		rep.Alarms = append(rep.Alarms, Alarm{AlarmRemoveFailed, fmt.Sprintf(
			"корт %s, %s: отражение %s в %s не удалось снять (%s) — сверка повторит",
			r.it.Court, p.span(r.it.Start, r.it.End), r.it.ID, r.dst.name, errText(err))})
		return false
	}
	p.log.Info("reconciliation: mirror removed", "court", r.it.Court, "crm", r.dst.typ, "mirror", r.it.ID,
		"start", r.it.Start.UTC().Format(time.RFC3339), "end", r.it.End.UTC().Format(time.RFC3339))
	rep.Removed = append(rep.Removed, r.it)
	return true
}
