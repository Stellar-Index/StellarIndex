package timescale

import "strings"

// sqlContainsFold reports whether q contains substr case-insensitively.
// "must not be window-bounded" shape guards check for the literal `interval`
// keyword; a case-sensitive check misses a capitalized `INTERVAL '...'` clause,
// the house spelling used everywhere else in this package.
func sqlContainsFold(q, substr string) bool {
	return strings.Contains(strings.ToLower(q), strings.ToLower(substr))
}
