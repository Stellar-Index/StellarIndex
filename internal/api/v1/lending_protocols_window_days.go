package v1

import "time"

// coveredDays is the UTC days [floor, through] a pool's stored rollup vouches for.
type coveredDays struct{ floor, through time.Time }

// coveredSpan is a protocol block's day range: through the latest covered day of any pool, from the earliest floor. A
// pool covered through an earlier day is named for the days it lacks, so the latest day is never hidden by the slowest.
func coveredSpan(cs []coveredDays) (floor, through time.Time, ok bool) {
	for _, c := range cs {
		if !ok || c.floor.Before(floor) {
			floor = c.floor
		}
		if !ok || c.through.After(through) {
			through = c.through
		}
		ok = true
	}
	return floor, through, ok
}

// protocolWindowFrom is the first day of window i of protocolRevenueWindows ending at through; all_time starts at floor.
func protocolWindowFrom(i int, floor, through time.Time) time.Time {
	if days := protocolRevenueWindows[i]; days > 0 {
		return through.Add(-time.Duration(days-1) * day24)
	}
	return floor
}

// overlap is how many days of [from, through] c covers and how many it does not.
func (c coveredDays) overlap(from, through time.Time) (in, missing int64) {
	lo, hi := maxTime(from, c.floor), minTime(through, c.through)
	in = max(int64(hi.Sub(lo)/day24)+1, 0)
	return in, int64(through.Sub(from)/day24) + 1 - in
}
