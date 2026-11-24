// Package swrverdict holds the one stale verdict the API's stale-while-revalidate caches serve by.
package swrverdict

import "time"

// Stale is the verdict for a value served past ttl: clean only while its age is within ttl plus one refresh (at most one
// more ttl, capped at refreshBudget) and its last completed refresh did not fail, panic or get skipped.
func Stale(age, ttl, refreshBudget time.Duration, refreshFailed bool) bool {
	return refreshFailed || age > ttl+min(ttl, refreshBudget)
}
