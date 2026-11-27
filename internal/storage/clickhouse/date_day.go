package clickhouse

import "time"

// dateDay is the UTC midnight of the day a scanned Date or day-valued time names. A Date decodes at local midnight in the
// server's zone, which Go moves into the previous day where the clocks jump forward at midnight, so the day is read
// twelve hours later in the value's own zone. On a UTC-tagged DateTime it changes nothing: there it is defence only.
func dateDay(t time.Time) time.Time {
	y, m, d := t.Add(12 * time.Hour).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
