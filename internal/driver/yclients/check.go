package yclients

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// syncHorizon is how far ahead PadelApp sells a YCLIENTS club's time
// (CRM_SYNC_HORIZON_DAYS of the backend): a court's schedule must reach it.
const syncHorizon = 14 * 24 * time.Hour

// company is GET /company/{id}/: the fields with which a branch limits
// what the gateway may do (D11 REQ-139).
type company struct {
	ID                 json.Number     `json:"id"`
	Title              string          `json:"title"`
	Active             json.RawMessage `json:"active"`
	TimetableOff       *bool           `json:"timetable_off"`
	TimezoneName       string          `json:"timezone_name"`
	AllowChangePrepaid *bool           `json:"allow_change_prepaid_record"`
	AllowDeletePrepaid *bool           `json:"allow_delete_prepaid_record"`
}

// staffMember is a court as GET /company/{id}/staff lists it.
type staffMember struct {
	ID           json.Number     `json:"id"`
	Name         string          `json:"name"`
	Bookable     *bool           `json:"bookable"`
	IsBookable   *bool           `json:"is_bookable"`
	Fired        json.RawMessage `json:"fired"`
	IsFired      *bool           `json:"is_fired"`
	IsDeleted    *bool           `json:"is_deleted"`
	Hidden       json.RawMessage `json:"hidden"`
	Prepaid      string          `json:"prepaid"`
	ScheduleTill *string         `json:"schedule_till"`
}

// truthy reads YCLIENTS' 0/1, true/false and "1".
func truthy(r json.RawMessage) bool {
	s := strings.Trim(strings.TrimSpace(string(r)), `"`)
	return s == "1" || s == "true"
}

func (d *Driver) readCompany(ctx context.Context) (*company, error) {
	r, err := d.c.do(ctx, http.MethodGet, "/company/"+d.company+"/", nil, nil, partnerAuth)
	if err != nil {
		return nil, err
	}
	switch r.status {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, &driver.MisconfigError{Problems: []string{fmt.Sprintf("crm.company_id: филиала %s в YCLIENTS нет", d.company)}}
	default:
		return nil, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /company: HTTP %d: %s", r.status, r.message()))
	}
	var c company
	if json.Unmarshal(r.data, &c) != nil || c.ID.String() != d.company {
		return nil, driver.Upstream(driver.UpstreamInvalid, "GET /company: not the branch asked for")
	}
	return &c, nil
}

func (d *Driver) readStaff(ctx context.Context) ([]staffMember, error) {
	r, err := d.c.do(ctx, http.MethodGet, "/company/"+d.company+"/staff", nil, nil, userAuth)
	if err != nil {
		return nil, err
	}
	if r.status != http.StatusOK {
		return nil, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /company/staff: HTTP %d: %s", r.status, r.message()))
	}
	var list []staffMember
	if len(r.data) == 0 || r.data[0] != '[' || json.Unmarshal(r.data, &list) != nil {
		return nil, driver.Upstream(driver.UpstreamInvalid, "GET /company/staff: not a list")
	}
	return list, nil
}

// CheckConfig implements driver.ConfigChecker with the D11 restriction
// check: what makes bookings impossible or wrong is a *MisconfigError (the
// branch inactive or its journal off, another time zone than the club's —
// schedule times are read in the club's —, a court that is no staff of
// the branch, fired, closed for booking, without a schedule, or with
// prepayment required, which lets the branch drop an unpaid record by
// itself); what may refuse some calls is a *ConfigAlert (a court hidden
// from online booking, a schedule shorter than the selling horizon); the
// rest is logged.
func (d *Driver) CheckConfig(ctx context.Context) error {
	co, err := d.readCompany(ctx)
	if err != nil {
		return err
	}
	var problems, alerts []string
	var alertCode string
	alert := func(code, msg string) {
		if alertCode == "" {
			alertCode = code
		}
		alerts = append(alerts, msg)
	}
	title := cmpOr(co.Title, d.company)
	if len(co.Active) > 0 && !truthy(co.Active) {
		problems = append(problems, fmt.Sprintf("филиал «%s» в YCLIENTS неактивен — брони в него не принимаются", title))
	}
	if co.TimetableOff != nil && *co.TimetableOff {
		problems = append(problems, fmt.Sprintf("у филиала «%s» в YCLIENTS выключен журнал записи — брони создать нельзя", title))
	}
	now, _ := d.clock()
	if co.TimezoneName != "" {
		if loc, err := time.LoadLocation(co.TimezoneName); err != nil {
			d.log.Warn("yclients branch time zone unknown to the gateway", "zone", co.TimezoneName)
		} else if at, differ := driver.ZonesDiffer(d.loc, loc, now, driver.ZoneHorizon); differ {
			problems = append(problems, fmt.Sprintf("timezone: часовой пояс клуба %s с %s расходится с поясом филиала YCLIENTS %s — расписание кортов легло бы со сдвигом: задайте клубу пояс филиала",
				d.loc, at.In(d.loc).Format("2006-01-02"), co.TimezoneName))
		}
	}
	if co.AllowDeletePrepaid != nil && !*co.AllowDeletePrepaid {
		d.log.Warn("yclients branch forbids deleting prepaid records: a cancel may be refused", "company", d.company)
	}
	if co.AllowChangePrepaid != nil && !*co.AllowChangePrepaid {
		d.log.Warn("yclients branch forbids changing prepaid records: the paid mark may be refused", "company", d.company)
	}
	staff, err := d.readStaff(ctx)
	if err != nil {
		return err
	}
	byID := map[string]staffMember{}
	for _, s := range staff {
		byID[s.ID.String()] = s
	}
	horizon := localDay(now.Add(syncHorizon), d.loc).Format("2006-01-02")
	for i, court := range d.courts {
		id := d.byCourt[court]
		path := fmt.Sprintf("courts[%d].%s", i, RoomKey)
		s, ok := byID[id]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: сотрудника-корта %s нет в филиале %s", path, id, d.company))
			continue
		}
		name := cmpOr(s.Name, id)
		if (s.IsFired != nil && *s.IsFired) || truthy(s.Fired) || (s.IsDeleted != nil && *s.IsDeleted) {
			problems = append(problems, fmt.Sprintf("%s: корт «%s» в YCLIENTS уволен или удалён", path, name))
			continue
		}
		if b := s.IsBookable; b != nil && !*b || b == nil && s.Bookable != nil && !*s.Bookable {
			problems = append(problems, fmt.Sprintf("%s: корт «%s» в YCLIENTS закрыт для записи", path, name))
		}
		switch till := ""; {
		case s.ScheduleTill != nil && dateRe.MatchString(*s.ScheduleTill) && *s.ScheduleTill != "0000-00-00" && *s.ScheduleTill < horizon:
			till = *s.ScheduleTill
			alert("SCHEDULE_SHORT", fmt.Sprintf("%s: расписание корта «%s» в YCLIENTS заполнено только до %s, а продаётся до %s — дальше корт занят; продлите расписание", path, name, till, horizon))
		case s.ScheduleTill == nil || !dateRe.MatchString(*s.ScheduleTill) || *s.ScheduleTill == "0000-00-00":
			problems = append(problems, fmt.Sprintf("%s: у корта «%s» в YCLIENTS не заполнено расписание", path, name))
		}
		if truthy(s.Hidden) {
			alert("STAFF_HIDDEN", fmt.Sprintf("%s: корт «%s» скрыт из онлайн-записи YCLIENTS — запись через API может быть отклонена", path, name))
		}
		switch s.Prepaid {
		case "", "allowed":
		case "forbidden":
			d.log.Warn("yclients court forbids online prepayment: the payment shows only as the paid mark", "staff", id)
		default:
			problems = append(problems, fmt.Sprintf("%s: у корта «%s» в YCLIENTS требуется онлайн-предоплата (%s) — запись без оплаты в YCLIENTS филиал может снять сам; переключите предоплату на «разрешена» или «запрещена»", path, name, s.Prepaid))
		}
	}
	if len(problems) > 0 {
		return &driver.MisconfigError{Problems: problems}
	}
	if len(alerts) > 0 {
		return &driver.ConfigAlert{Code: alertCode, Problems: alerts}
	}
	return nil
}

func isAlert(err error) bool {
	var ca *driver.ConfigAlert
	return errors.As(err, &ca)
}

func cmpOr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return strings.TrimSpace(a)
	}
	return b
}
