package v1

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/swrverdict"
)

// swrServedStale is the [swrverdict.Stale] verdict for a value filled at filledAt and served now: fresh inside ttl, then clean
// only while its refresh is on time and the last one finished did not fail.
func swrServedStale(filledAt time.Time, ttl, refreshBudget time.Duration, refreshFailed bool) bool {
	age := time.Since(filledAt)
	return age >= ttl && swrverdict.Stale(age, ttl, refreshBudget, refreshFailed)
}
