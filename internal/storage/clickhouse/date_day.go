package clickhouse

import "time"

// dateDay is the UTC midnight of the calendar day a scanned Date names. The driver decodes a Date at local midnight in
// the server's (or the query's) zone; where the clocks jump forward at midnight that instant does not exist and Go moves
// it into the previous day, so the day is read twelve hours later, in the value's own zone.
func dateDay(t time.Time) time.Time {
	y, m, d := t.Add(12 * time.Hour).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
