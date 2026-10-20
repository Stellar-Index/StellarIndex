package forex

import (
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Currency is one snapshot entry: ticker, display name and price of 1 USD in this currency; the Cache replaces snapshots atomically.
type Currency struct {
	Ticker  string  // upper-case ISO-4217 (USD, EUR, JPY, …)
	Name    string  // display name ("United States Dollar")
	RateUSD float64 //floatmoney:ok known debt — same chain as worker.go/fx_quotes.go RateUSD; 1 USD = N units of this currency
	// UpdateAt is the publication time of the refresh whose rate this
	// is. It trails [Snapshot.PublishedAt] when the worker is HOLDING the
	// ticker's last guarded rate (see servedSnapshot in worker.go).
	UpdateAt time.Time
	// Source is the feed that published RateUSD. A held entry keeps its
	// original feed, so one snapshot can mix the primary and a standby.
	Source string
}

// Snapshot is the immutable rates+metadata bundle the Cache holds, replaced atomically; it carries only rates the sanity band cleared, and History7d is the 7-day per-ticker series (empty until backfill completes).
type Snapshot struct {
	Currencies  []Currency
	PublishedAt time.Time
	FetchedAt   time.Time
	History7d   map[string][]HistoryPoint
	// Circulation is the curated monetary-base table loaded once at worker startup from the embedded CSV. Indexed by lower-case
	// ticker. Stays nil until the worker installs the first snapshot — readers MUST nil-check before lookup.
	Circulation map[string]CirculationEntry
}

// HistoryPoint is one daily rate datum for the 7d series.
type HistoryPoint struct {
	Date time.Time // YYYY-MM-DD UTC
	//floatmoney:ok known debt — same chain as Currency.RateUSD above; 1 USD = N units on that date
	RateUSD float64
}

// Cache holds the latest forex snapshot, safe for concurrent use via atomic.Pointer; readers MUST handle the nil no-snapshot-yet case.
type Cache struct {
	snapshot atomic.Pointer[Snapshot]
}

// NewCache returns an empty Cache that the worker populates on first successful fetch.
func NewCache() *Cache {
	return &Cache{}
}

// Latest returns the most recent snapshot, or nil if no fetch has completed; callers MUST nil-check.
func (c *Cache) Latest() *Snapshot {
	return c.snapshot.Load()
}

// Set installs a new snapshot atomically.
func (c *Cache) Set(s *Snapshot) {
	c.snapshot.Store(s)
}

// anchorTicker is the base currency buildSnapshot synthesises at 1.0;
// its row carries no upstream information.
const anchorTicker = "USD"

// buildSnapshot merges a rates map and a names map into the public Currency slice, sorted by ticker. USD (rate 1.0 implied) and any
// code without a name are excluded. The join is CASE-INSENSITIVE: the primary client lower-cases both maps, the ECB standby is UPPER,
// and an exact-case join would collapse a degraded refresh to the synthetic USD row.
func buildSnapshot(rates map[string]float64, names map[string]string, publishedAt, fetchedAt time.Time, history map[string][]HistoryPoint, circulation map[string]CirculationEntry) *Snapshot {
	rates, names = lowerKeyed(rates), lowerKeyed(names)
	out := make([]Currency, 0, len(rates)+1)
	// Always-include USD as the base. Rate is 1.0 by definition;
	// the name comes from the names map (fallback to "US Dollar").
	usdName := names["usd"]
	if usdName == "" {
		usdName = "US Dollar"
	}
	out = append(out, Currency{
		Ticker:   anchorTicker,
		Name:     toTitle(usdName),
		RateUSD:  1.0,
		UpdateAt: publishedAt,
	})
	for code, rate := range rates {
		if rate <= 0 || !isFiniteFloat(rate) {
			continue
		}
		name := names[code]
		if name == "" {
			continue
		}
		ticker := strings.ToUpper(code)
		if ticker == anchorTicker {
			continue
		}
		out = append(out, Currency{
			Ticker:   ticker,
			Name:     toTitle(name),
			RateUSD:  rate,
			UpdateAt: publishedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ticker < out[j].Ticker })
	if history == nil {
		history = map[string][]HistoryPoint{}
	}
	return &Snapshot{
		Currencies:  out,
		PublishedAt: publishedAt,
		FetchedAt:   fetchedAt,
		History7d:   history,
		Circulation: circulation,
	}
}

// lowerKeyed re-keys m by lower-case currency code, the one form every rates/names join here uses. Keys differing only by case resolve
// to the one that sorts first, so the result never depends on map iteration order.
func lowerKeyed[V any](m map[string]V) map[string]V {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]V, len(m))
	for _, k := range keys {
		lk := strings.ToLower(k)
		if _, dup := out[lk]; dup {
			continue
		}
		out[lk] = m[k]
	}
	return out
}

func isFiniteFloat(f float64) bool {
	// The old `f != f+1` inf check is wrong for |f| >= 2^53, where
	// f+1 rounds back to f and a perfectly finite value reads as
	// non-finite. Use the standard library predicates.
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

// toTitle title-cases an upstream currency name so output is deterministic across snapshots.
func toTitle(s string) string {
	parts := strings.Fields(s)
	for i, p := range parts {
		if p == "" {
			continue
		}
		if len(p) <= 2 {
			parts[i] = strings.ToUpper(p)
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
	}
	return strings.Join(parts, " ")
}
