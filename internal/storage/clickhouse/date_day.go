package clickhouse

import "time"

// dateDay is the UTC midnight of a scanned Date column's calendar day. The driver builds a Date as local midnight in
// the server's (or query's) zone, which Go moves to 23:00 the previous day where the clocks jump at midnight; noon of
// the decoded value names the day in every zone.
func dateDay(t time.Time) time.Time {
	y, m, d := t.Add(12 * time.Hour).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
