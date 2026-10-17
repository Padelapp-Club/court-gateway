package yclients

// Tests do not wait out the rate limit of a live YCLIENTS.
func init() { minInterval = 0 }

// MinInterval exposes the spacing for the throttle test.
var MinInterval = &minInterval
