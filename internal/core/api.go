package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// Response DTOs. Every response is built from these whitelisted structs
// only; nothing from a CRM response is ever passed through.

type hoursDTO struct {
	Open  string `json:"open"`
	Close string `json:"close"`
}

type capabilitiesDTO struct {
	Payments bool `json:"payments"`
}

type clubDTO struct {
	ID           string               `json:"id"`
	Name         string               `json:"name"`
	Timezone     string               `json:"timezone"`
	SlotMinutes  int                  `json:"slot_minutes"`
	Hours        map[string]*hoursDTO `json:"hours"`
	Capabilities capabilitiesDTO      `json:"capabilities"`
}

type courtDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type intervalDTO struct {
	ID    string `json:"id"`
	Court string `json:"court"`
	Start string `json:"start"`
	End   string `json:"end"`
}

type occupancyDTO struct {
	Complete  bool          `json:"complete"`
	Intervals []intervalDTO `json:"intervals"`
}

type periodDTO struct {
	Start        string `json:"start"`
	End          string `json:"end"`
	PricePerHour int64  `json:"price_per_hour"`
}

type pricesDTO struct {
	Court    string      `json:"court"`
	Date     string      `json:"date"`
	Currency string      `json:"currency"`
	Periods  []periodDTO `json:"periods"`
}

type quoteDTO struct {
	Court    string `json:"court"`
	Start    string `json:"start"`
	End      string `json:"end"`
	Currency string `json:"currency"`
	Total    int64  `json:"total"`
}

type bookingDTO struct {
	ID     string `json:"id"`
	Court  string `json:"court"`
	Start  string `json:"start"`
	End    string `json:"end"`
	Kind   string `json:"kind,omitempty"`
	Status string `json:"status"`
}

type createRequest struct {
	Court string `json:"court"`
	Start string `json:"start"`
	End   string `json:"end"`
	Kind  string `json:"kind"`
	Note  string `json:"note"`
}

const (
	maxOccupancyRange = 31 * 24 * time.Hour
	maxBookingLength  = 24 * time.Hour
	maxNoteRunes      = 500
	maxBodyBytes      = 16 << 10
)

var bookingIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

func (s *Server) ts(t time.Time) string { return t.In(s.cfg.Location).Format(time.RFC3339) }

func (s *Server) handleClub(w http.ResponseWriter, _ *http.Request) {
	hours := make(map[string]*hoursDTO, 7)
	for i, h := range s.cfg.Hours {
		if h == nil {
			hours[dayKeys[i]] = nil
			continue
		}
		hours[dayKeys[i]] = &hoursDTO{Open: FormatClock(h[0]), Close: FormatClock(h[1])}
	}
	WriteJSON(w, http.StatusOK, clubDTO{
		ID:           s.cfg.ClubID,
		Name:         s.cfg.ClubName,
		Timezone:     s.cfg.TimezoneName,
		SlotMinutes:  s.cfg.SlotMinutes,
		Hours:        hours,
		Capabilities: capabilitiesDTO{Payments: s.caps.Payments},
	})
}

func (s *Server) handleCourts(w http.ResponseWriter, _ *http.Request) {
	out := make([]courtDTO, 0, len(s.cfg.Courts))
	for _, c := range s.cfg.Courts {
		out = append(out, courtDTO{ID: c.ID, Name: c.Name})
	}
	WriteJSON(w, http.StatusOK, map[string][]courtDTO{"courts": out})
}

func (s *Server) courtParam(w http.ResponseWriter, r *http.Request, required bool) (string, bool) {
	c := r.URL.Query().Get("court")
	if c == "" {
		if required {
			WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "query parameter court is required")
			return "", false
		}
		return "", true
	}
	if _, ok := s.cfg.CourtByID(c); !ok {
		WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "no such court")
		return "", false
	}
	return c, true
}

func (s *Server) handleOccupancy(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, err1 := time.Parse(time.RFC3339, q.Get("from"))
	to, err2 := time.Parse(time.RFC3339, q.Get("to"))
	if err1 != nil || err2 != nil {
		WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "from and to are required RFC3339 timestamps")
		return
	}
	if !to.After(from) || to.Sub(from) > maxOccupancyRange {
		WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "to must be after from and the range must not exceed 31 days")
		return
	}
	court, ok := s.courtParam(w, r, false)
	if !ok {
		return
	}
	courts := []string{court}
	if court == "" {
		courts = courts[:0]
		for _, c := range s.cfg.Courts {
			courts = append(courts, c.ID)
		}
	}
	occ, err := s.occupancy(r.Context(), courts, from, to)
	if err != nil {
		s.driverError(w, r, "occupancy", err)
		return
	}
	out := occupancyDTO{Complete: occ.Complete, Intervals: make([]intervalDTO, 0, len(occ.Intervals))}
	for _, iv := range occ.Intervals {
		out.Intervals = append(out.Intervals, intervalDTO{ID: iv.ID, Court: iv.Court, Start: s.ts(iv.Start), End: s.ts(iv.End)})
	}
	WriteJSON(w, http.StatusOK, out)
}

// occupancy calls the driver and re-applies the core's own guarantees:
// only requested courts, only intervals overlapping [from, to), sorted.
func (s *Server) occupancy(ctx context.Context, courts []string, from, to time.Time) (driver.Occupancy, error) {
	ctx, cancel := context.WithTimeout(ctx, driverTimeout)
	defer cancel()
	occ, err := s.drv.Occupancy(ctx, driver.OccupancyQuery{Courts: courts, From: from, To: to})
	if err != nil {
		return driver.Occupancy{}, err
	}
	want := make(map[string]bool, len(courts))
	for _, c := range courts {
		want[c] = true
	}
	kept := occ.Intervals[:0:0]
	for _, iv := range occ.Intervals {
		if want[iv.Court] && iv.End.After(iv.Start) && iv.Start.Before(to) && iv.End.After(from) {
			kept = append(kept, iv)
		}
	}
	sort.Slice(kept, func(i, j int) bool {
		if !kept[i].Start.Equal(kept[j].Start) {
			return kept[i].Start.Before(kept[j].Start)
		}
		if kept[i].Court != kept[j].Court {
			return kept[i].Court < kept[j].Court
		}
		return kept[i].ID < kept[j].ID
	})
	occ.Intervals = kept
	return occ, nil
}

func (s *Server) priceTable(ctx context.Context, court string) (driver.PriceTable, error) {
	ctx, cancel := context.WithTimeout(ctx, driverTimeout)
	defer cancel()
	return s.drv.PriceTable(ctx, court)
}

func (s *Server) handlePrices(w http.ResponseWriter, r *http.Request) {
	court, ok := s.courtParam(w, r, true)
	if !ok {
		return
	}
	date, err := time.ParseInLocation("2006-01-02", r.URL.Query().Get("date"), s.cfg.Location)
	if err != nil {
		WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "date is required in YYYY-MM-DD format")
		return
	}
	table, err := s.priceTable(r.Context(), court)
	if err != nil {
		s.driverError(w, r, "prices", err)
		return
	}
	out := pricesDTO{Court: court, Date: date.Format("2006-01-02"), Currency: s.caps.Currency, Periods: []periodDTO{}}
	for _, p := range datePeriods(table, s.cfg.Location, date) {
		out.Periods = append(out.Periods, periodDTO{Start: FormatClock(p.From), End: FormatClock(p.To), PricePerHour: rubles(p.PerHour)})
	}
	WriteJSON(w, http.StatusOK, out)
}

func (s *Server) handleQuote(w http.ResponseWriter, r *http.Request) {
	court, ok := s.courtParam(w, r, true)
	if !ok {
		return
	}
	q := r.URL.Query()
	start, err1 := time.Parse(time.RFC3339, q.Get("start"))
	end, err2 := time.Parse(time.RFC3339, q.Get("end"))
	if err1 != nil || err2 != nil || !end.After(start) || end.Sub(start) > maxBookingLength {
		WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "start and end are required RFC3339 timestamps, end after start, at most 24 hours")
		return
	}
	table, err := s.priceTable(r.Context(), court)
	if err != nil {
		s.driverError(w, r, "quote", err)
		return
	}
	total, err := quote(r.Context(), table, s.cfg.Location, s.cfg.SlotMinutes, start, end)
	switch {
	case err != nil && r.Context().Err() != nil:
		s.driverError(w, r, "quote", err)
		return
	case errors.Is(err, errNotAligned):
		WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "start and end must be on the club's slot grid")
		return
	case err != nil:
		WriteError(w, r, http.StatusUnprocessableEntity, "PRICE_UNAVAILABLE", "no price is defined for part of the interval")
		return
	}
	WriteJSON(w, http.StatusOK, quoteDTO{Court: court, Start: s.ts(start), End: s.ts(end), Currency: s.caps.Currency, Total: total})
}

// cleanNote drops control characters (except newlines and tabs) and trims.
func cleanNote(n string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, n))
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	partner := info(r).partner
	var req createRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "body must be a JSON object with court, start, end, kind and optional note")
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "body must contain a single JSON object")
		return
	}
	if _, ok := s.cfg.CourtByID(req.Court); !ok {
		WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "no such court")
		return
	}
	start, err1 := time.Parse(time.RFC3339, req.Start)
	end, err2 := time.Parse(time.RFC3339, req.End)
	if err1 != nil || err2 != nil || !end.After(start) || end.Sub(start) > maxBookingLength {
		WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "start and end are required RFC3339 timestamps, end after start, at most 24 hours")
		return
	}
	if !aligned(start, s.cfg.Location, s.cfg.SlotMinutes) || !aligned(end, s.cfg.Location, s.cfg.SlotMinutes) {
		WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "start and end must be on the club's slot grid")
		return
	}
	now, known := s.clock()
	if !known {
		w.Header().Set("Retry-After", "5")
		s.driverError(w, r, "create", driver.ErrClockUnknown)
		return
	}
	if !start.After(now) {
		WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "start must be in the future")
		return
	}
	kind := driver.Kind(req.Kind)
	if kind != driver.KindGame && kind != driver.KindTournament {
		WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "kind must be game or tournament")
		return
	}
	note := cleanNote(req.Note)
	if utf8.RuneCountInString(note) > maxNoteRunes {
		WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "note must not exceed 500 characters")
		return
	}

	// no other create of this court may run between our check and our write
	lctx, lcancel := context.WithTimeout(r.Context(), createLockWait)
	unlock, err := createLocks.lock(lctx, resourceKey(s.drv, req.Court))
	lcancel()
	if err != nil {
		w.Header().Set("Retry-After", "5")
		WriteError(w, r, http.StatusServiceUnavailable, "BUSY", "another booking of this court is in progress, retry")
		return
	}
	defer unlock()
	occ, err := s.occupancy(r.Context(), []string{req.Court}, start, end)
	if err != nil {
		s.driverError(w, r, "create.check", err)
		return
	}
	if !occ.Complete {
		WriteError(w, r, http.StatusConflict, "OCCUPANCY_UNKNOWN", "the occupancy of this interval cannot be confirmed")
		return
	}
	if len(occ.Intervals) > 0 {
		WriteError(w, r, http.StatusConflict, "SLOT_UNAVAILABLE", "the court is occupied in this interval")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), driverTimeout)
	defer cancel()
	b, err := s.drv.Create(ctx, driver.CreateRequest{Court: req.Court, Start: start, End: end, Kind: kind, Note: note, Partner: partner})
	if err != nil {
		s.driverError(w, r, "create", err)
		return
	}
	s.log.Info("booking created", "request_id", info(r).id, "partner", partner, "booking", b.ID, "court", req.Court)
	w.Header().Set("Location", "/v1/clubs/"+s.cfg.ClubID+"/bookings/"+b.ID)
	WriteJSON(w, http.StatusCreated, bookingDTO{
		ID: b.ID, Court: req.Court, Start: s.ts(start), End: s.ts(end), Kind: string(kind), Status: string(driver.StatusActive),
	})
}

func (s *Server) bookingID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !bookingIDRe.MatchString(id) {
		WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "no such booking")
		return "", false
	}
	return id, true
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bookingID(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), driverTimeout)
	defer cancel()
	b, err := s.drv.Get(ctx, id)
	if err != nil {
		s.driverError(w, r, "get", err)
		return
	}
	if _, known := s.cfg.CourtByID(b.Court); !known {
		s.driverError(w, r, "get", driver.ErrCourtNotConfigured)
		return
	}
	status := b.Status
	if status != driver.StatusCancelled {
		status = driver.StatusActive
	}
	WriteJSON(w, http.StatusOK, bookingDTO{ID: b.ID, Court: b.Court, Start: s.ts(b.Start), End: s.ts(b.End), Status: string(status)})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bookingID(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), driverTimeout)
	defer cancel()
	if err := s.drv.Cancel(ctx, id, info(r).partner); err != nil {
		s.driverError(w, r, "cancel", err)
		return
	}
	s.log.Info("booking cancelled", "request_id", info(r).id, "partner", info(r).partner, "booking", id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePayment(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bookingID(w, r)
	if !ok {
		return
	}
	if !s.caps.Payments {
		WriteError(w, r, http.StatusNotImplemented, "NOT_SUPPORTED", "the club's CRM does not support payment marks")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), driverTimeout)
	defer cancel()
	if err := s.drv.MarkPaid(ctx, id, info(r).partner); err != nil {
		s.driverError(w, r, "payment", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
