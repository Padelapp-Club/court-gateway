package yclients_test

import (
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/yclients/yclientsstub"
)

// The form doctor takes as a driver's own config alert code
// (doctor.alertCode).
var doctorAlertCode = regexp.MustCompile(`^[A-Z][A-Z_]{2,40}$`)

// What may refuse some calls is a ConfigAlert — the club served, the code
// shown — and a misconfiguration still wins over it.
func TestConfigAlerts(t *testing.T) {
	short := time.Now().AddDate(0, 0, 5).Format("2006-01-02")
	for _, c := range []struct {
		name     string
		set      func(*yclientsstub.Staff)
		code     string
		problems int
	}{
		{"hidden court", func(s *yclientsstub.Staff) { s.Hidden = true }, "STAFF_HIDDEN", 1},
		{"short schedule", func(s *yclientsstub.Staff) { s.ScheduleTill = short }, "SCHEDULE_SHORT", 1},
		{"both: the first is the code", func(s *yclientsstub.Staff) { s.Hidden, s.ScheduleTill = true, short }, "SCHEDULE_SHORT", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, ycTileCourt(opts{}, c.set))
			for name, check := range map[string]func() error{
				"CheckConfig": func() error { return f.drv.CheckConfig(ycCtx) },
				"Ready":       func() error { return f.drv.Ready(ycCtx) },
			} {
				var ca *driver.ConfigAlert
				err := check()
				if !errors.As(err, &ca) || ca.Code != c.code || len(ca.Problems) != c.problems {
					t.Fatalf("%s = %v, want ConfigAlert %s with %d problems", name, err, c.code, c.problems)
				}
				if !doctorAlertCode.MatchString(ca.Code) {
					t.Fatalf("%s: code %q is not of the form doctor knows", name, ca.Code)
				}
			}
		})
	}
	t.Run("a misconfiguration wins", func(t *testing.T) {
		f := newFixture(t, ycTileCourt(opts{}, func(s *yclientsstub.Staff) { s.Hidden, s.Prepaid = true, "required" }))
		var me *driver.MisconfigError
		if err := f.drv.CheckConfig(ycCtx); !errors.As(err, &me) {
			t.Fatalf("err = %v, want MisconfigError", err)
		}
	})
}
