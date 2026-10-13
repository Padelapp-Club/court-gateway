package driver

// Weekdays is a set of ISO weekdays: index 0 = Monday ... 6 = Sunday.
type Weekdays [7]bool

// AllWeekdays is the full week.
var AllWeekdays = Weekdays{true, true, true, true, true, true, true}

// Has reports whether the set contains the weekday given as an ISO index
// (0 = Monday).
func (w Weekdays) Has(isoIndex int) bool { return isoIndex >= 0 && isoIndex < 7 && w[isoIndex] }

// PriceRule is a base hourly price on some weekdays within [From, To) minutes
// of the local day. To may be 1440 (midnight at the end of the day).
type PriceRule struct {
	Days Weekdays
	From int // minutes since local midnight
	To   int // minutes since local midnight, From < To <= 1440
	// PerHour is the price of one hour in kopecks (1/100 of a ruble).
	PerHour int64
}

// PriceAdjustment adds (or, if negative, subtracts) an hourly amount on top
// of the base price within a window.
type PriceAdjustment struct {
	Days         Weekdays
	From         int
	To           int
	PerHourDelta int64 // kopecks per hour, may be negative
}

// PriceTable is a court's complete price model. For a slot the first base
// rule that fully contains it gives the price; every adjustment that fully
// contains it is then added.
type PriceTable struct {
	Base        []PriceRule
	Adjustments []PriceAdjustment
}
