package timescale

import "strings"

// sqlContainsFold reports whether q contains substr case-insensitively; the "must not be window-bounded"
// guards look for the literal `interval`, which a case-sensitive check misses in the package's house
// `INTERVAL '...'`.
func sqlContainsFold(q, substr string) bool {
	return strings.Contains(strings.ToLower(q), strings.ToLower(substr))
}
