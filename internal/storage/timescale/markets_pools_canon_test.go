package timescale

import (
	"strings"
	"testing"
	"time"
)

// Both /v1/pools orderings must read the SAME canonical CTE.
//
// The pair-ordered tail used to select `FROM pools` — the pre-collapse
// CTE — while the volume-desc tail selected `FROM canon`. So the two
// orderings disagreed about what a pool is: `?order_by=pair` returned
// both orientations of every two-sided market as separate rows, each
// carrying only its own direction's vol_24h_usd and count_24h rather
// than the summed pair, with last_price un-inverted on the flipped
// side. Measured on r1 2026-08-03: 61 duplicate both-orientation pairs
// in a 200-row page versus 0 on the default ordering, and both variants
// cache under distinct keys so the disagreement was durable.
func TestBuildPoolsQuery_BothOrderingsSelectFromCanon(t *testing.T) {
	t.Parallel()

	since := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	for name, order := range map[string]MarketsOrder{
		"volume desc": MarketsOrderVolume24hDesc,
		"pair":        MarketsOrderPair,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q, _ := buildPoolsQuery(since, PoolsFilter{}, "", 100, order)

			// The final SELECT — everything after the last CTE — must
			// read `canon`. Checking the suffix rather than the whole
			// string, since `canon` itself legitimately selects FROM
			// pools.
			tail := q[strings.LastIndex(q, "SELECT source, base_asset"):]
			if !strings.Contains(tail, "FROM canon") {
				t.Errorf("%s ordering does not select FROM canon — it returns both "+
					"orientations of each market with un-summed volumes:\n%s", name, tail)
			}
			if strings.Contains(tail, "FROM pools") {
				t.Errorf("%s ordering selects the pre-collapse CTE:\n%s", name, tail)
			}
		})
	}
}
