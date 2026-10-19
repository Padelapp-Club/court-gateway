package driver

import (
	"context"
	"regexp"
	"strings"
	"time"
)

// An installation may hold a second CRM of the same clubs, the mirror
// (Lunda next to Viva, YCLIENTS or 1C:Fitness): the core then serves a
// club through a pair of drivers (internal/pair), and a reconciliation
// loop blocks in each CRM what was booked directly in the other. A driver
// takes part in a pair by implementing Mirrorer: it tells the loop what is
// on its courts and whose it is, and makes and removes the loop's blocks.
// Which block mirrors which booking is kept nowhere but in the blocks
// themselves (MirrorMarker), so nothing of it is stored by the gateway.

// MirrorClass is what an item of a CRM's schedule is to the loop.
type MirrorClass int

const (
	// ClassClub is the club's own booking or block (or busy time the CRM
	// does not explain, see MirrorItem.Opaque): it is mirrored.
	ClassClub MirrorClass = iota
	// ClassOwn is a booking a partner made through the gateway (its
	// ownership marker "[cg:<partner>]"): it is in both CRMs already and
	// never mirrored.
	ClassOwn
	// ClassMirror is a block the loop made for an item of the other CRM.
	ClassMirror
	// ClassDamaged starts like a mirror's marker but does not read as one:
	// nobody touches it, and it raises an alarm.
	ClassDamaged
	// ClassTransient occupies the court but is no booking (a hold of a
	// few minutes while someone pays): not mirrored.
	ClassTransient
)

func (c MirrorClass) String() string {
	switch c {
	case ClassClub:
		return "club"
	case ClassOwn:
		return "own"
	case ClassMirror:
		return "mirror"
	case ClassDamaged:
		return "damaged"
	case ClassTransient:
		return "transient"
	}
	return "unknown"
}

// MirrorMarker names the item a block mirrors: Src is the crm.type of the
// CRM it is in, ID its id as that driver gives it (an interval id). It is
// written, exactly and alone, as the comment of the block:
// "[cg-mirror:<src>:<id>]" — no personal data, nothing else.
type MirrorMarker struct {
	Src string
	ID  string
}

func (m MirrorMarker) String() string { return mirrorPrefix + ":" + m.Src + ":" + m.ID + "]" }

// mirrorPrefix starts every mirror's marker. It is not an ownership
// marker ("[cg:"): no partner's marker check ever takes a mirror for its
// booking.
const mirrorPrefix = "[cg-mirror"

var (
	mirrorRe = regexp.MustCompile(`^\[cg-mirror:(viva|yclients|fitness1c|lunda):([A-Za-z0-9_-]{1,100})\]$`)
	ownRe    = regexp.MustCompile(`^\[cg:([a-z0-9][a-z0-9_-]{0,31})\]`)
)

// MarkerState is how a comment reads as a mirror's marker.
type MarkerState int

const (
	// MarkerNone: the comment does not start with "[cg-mirror".
	MarkerNone MarkerState = iota
	// MarkerValid: the comment, trimmed, is exactly one marker.
	MarkerValid
	// MarkerDamaged: it starts with "[cg-mirror" but is not exactly one
	// marker (cut off, more text, a source of no known CRM).
	MarkerDamaged
)

// ParseMirrorMarker reads a comment as a mirror's marker. The id is taken
// as written and compared whole, case included, by its users.
func ParseMirrorMarker(text string) (MirrorMarker, MarkerState) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, mirrorPrefix) {
		return MirrorMarker{}, MarkerNone
	}
	m := mirrorRe.FindStringSubmatch(t)
	if m == nil {
		return MirrorMarker{}, MarkerDamaged
	}
	return MirrorMarker{Src: m[1], ID: m[2]}, MarkerValid
}

// OwnMarker reads the partner of a gateway booking's ownership marker at
// the start of a comment ("[cg:<partner>] <note>").
func OwnMarker(text string) (partner string, ok bool) {
	m := ownRe.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ClassOfComment classifies by its comment an item of a kind that may be
// the gateway's (the driver has checked the rest: the record's kind and
// client): a mirror's marker, damaged or not, or a partner's.
func ClassOfComment(text string) (MirrorClass, *MirrorMarker, string) {
	switch m, st := ParseMirrorMarker(text); st {
	case MarkerValid:
		return ClassMirror, &m, ""
	case MarkerDamaged:
		return ClassDamaged, nil, ""
	}
	if p, ok := OwnMarker(text); ok {
		return ClassOwn, nil, p
	}
	return ClassClub, nil, ""
}

// MirrorItem is one interval of a CRM's schedule (one court, one stretch),
// classified. A booking on several courts is several items of one ID.
type MirrorItem struct {
	ID    string
	Court string
	Start time.Time
	End   time.Time
	Class MirrorClass
	// Partner of a ClassOwn item.
	Partner string
	// Source of a ClassMirror item; nil where the CRM keeps no comment
	// and a block is known by its client only (1C:Fitness).
	Source *MirrorMarker
	// Opaque: busy time the CRM does not explain (a closed hour as well as
	// a booking): mirrored only within the club's hours.
	Opaque bool
	// Started: the CRM says the item is under way.
	Started bool
}

// Inventory is a classified read of courts.
type Inventory struct {
	// Complete is false when the CRM may not have returned every item
	// that starts in the range asked.
	Complete bool
	Items    []MirrorItem
	// Unmapped are the gateway's mirrors found on rooms of the CRM that no
	// court of the club maps (Court is the CRM's own id of the room): a
	// mapping changed under them (F6). The loop reports them and never
	// touches them. A driver lists those it can tell from what it reads
	// anyway (Lunda, 1C:Fitness), and reads nothing more for them.
	Unmapped []MirrorItem
}

// BlockRequest asks a driver to block a court for an item of the other
// CRM.
type BlockRequest struct {
	Court      string
	Start, End time.Time
	Source     MirrorMarker
}

// Mirrorer is implemented by drivers that can be a CRM of a pair. Every
// write it makes is a block of the gateway's own kind, told apart by its
// marker and by the record's class (a break, the service client, the
// mirror client), never a booking of the club; it removes only such.
type Mirrorer interface {
	// MirrorInventory lists every item of q.Courts that starts in
	// [q.From, q.To) (Complete: all of them), and may list more.
	MirrorInventory(ctx context.Context, q OccupancyQuery) (Inventory, error)
	// MirrorLookup reads one item again by its id (a point read): its
	// intervals as they are now, none when it is gone or cancelled.
	// ErrNotSupported when the CRM cannot read it by id.
	MirrorLookup(ctx context.Context, id string) ([]MirrorItem, error)
	// CreateBlock makes a block for req and returns it as the inventory
	// would read it.
	CreateBlock(ctx context.Context, req BlockRequest) (MirrorItem, error)
	// DeleteBlock removes a block read before, after reading it again: it
	// must still be the gateway's block of the same source, court and
	// span (ErrNotOwned otherwise, nothing removed). A block already gone
	// is no error.
	DeleteBlock(ctx context.Context, it MirrorItem) error
	// BlockSpan is the span a block for [start, end) takes (a CRM that
	// books whole seances rounds it out to its grid).
	BlockSpan(start, end time.Time) (time.Time, time.Time)
	// MirrorHorizon is how far ahead of now the CRM's schedule can be read
	// whole (a CRM that books only so many days ahead reads its setting);
	// a pass reads no further than the shorter horizon of its pair.
	MirrorHorizon(ctx context.Context) (time.Duration, error)
}

// MultiResourcer is implemented by drivers whose court is more than one
// CRM resource (a pair: a room of each CRM). The core locks them all.
type MultiResourcer interface {
	Resources(court string) []string
}

// MirrorError is a failure of the mirror CRM of a pair (its readiness,
// its half of a booking): readiness reports it as MIRROR_<reason>, so the
// operator knows which of the club's two CRMs it is about.
type MirrorError struct {
	Err error
}

func (e *MirrorError) Error() string { return "mirror CRM: " + e.Err.Error() }

func (e *MirrorError) Unwrap() error { return e.Err }
