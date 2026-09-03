package clickhouse

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// The /accounts hub reads four rollup tables. These classifiers match the
// exact statements accounts_stats.go emits; the schema PROBE also reads
// stellar.accounts_stats, so it is matched first by its LIMIT 1.
func isStatsProbe(q string) bool {
	return strings.Contains(q, "stellar.accounts_stats") && strings.Contains(q, "LIMIT 1")
}

func isStatsMetrics(q string) bool {
	return strings.Contains(q, "metric, value, computed_at") && strings.Contains(q, "stellar.accounts_stats")
}

func isWealthHistogram(q string) bool {
	return strings.Contains(q, "stellar.accounts_wealth_histogram")
}

func isTrustlineHistogram(q string) bool {
	return strings.Contains(q, "stellar.accounts_trustline_histogram")
}

func isTopHeldAssets(q string) bool {
	return strings.Contains(q, "stellar.asset_holders_counts")
}

// statsConn wires a full happy-path rollup snapshot.
func statsConn(t *testing.T, metrics, wealth, trust, held [][]any) (*ExplorerReader, *stubConn) {
	t.Helper()
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case isStatsProbe(q):
			return &stubRows{data: [][]any{{int64(1)}}}, nil
		case isStatsMetrics(q):
			return &stubRows{data: metrics}, nil
		case isWealthHistogram(q):
			return &stubRows{data: wealth}, nil
		case isTrustlineHistogram(q):
			return &stubRows{data: trust}, nil
		case isTopHeldAssets(q):
			return &stubRows{data: held}, nil
		}
		t.Fatalf("unexpected query: %s", q)
		return nil, nil
	}
	return &ExplorerReader{conn: conn}, conn
}

// TestAccountsStats_AssemblesTheFullSnapshot pins the metric-name → field
// mapping. That map is the whole of readStatsMetrics' correctness: the rollup
// delivers name/value PAIRS, so a renamed or mistyped key does not fail — it
// leaves the field at its zero value and the hub publishes "0 accounts" or a
// zero median as a measured fact.
func TestAccountsStats_AssemblesTheFullSnapshot(t *testing.T) {
	older := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	r, _ := statsConn(t,
		[][]any{
			{"total_accounts", int64(53_800_000), older},
			{"total_trustlines", int64(9_100_000), older},
			{"trustline_holding_accounts", int64(2_400_000), older},
			{"xlm_total_stroops", int64(1_050_000_000_000_000_000), newer},
			{"avg_stroops", int64(19_516_728), older},
			{"median_stroops", int64(15_000_000), older},
			{"p90_stroops", int64(310_000_000), older},
			{"p99_stroops", int64(9_900_000_000), older},
			{"top100_xlm_stroops", int64(420_000_000_000_000), older},
			// An unknown metric must be ignored, not error: the rollup can
			// gain a metric before this reader learns about it.
			{"some_future_metric", int64(1), older},
		},
		[][]any{
			{int8(-1), uint64(30_000_000), int64(1_000)},
			{int8(0), uint64(10_000_000), int64(50_000_000)},
		},
		[][]any{{"1", uint64(1_500_000)}, {"2-5", uint64(700_000)}},
		[][]any{{"USDC-" + testIssuer, int64(900_000)}, {"native", int64(53_000_000)}},
	)

	got, ok, err := r.AccountsStats(t.Context())
	if err != nil {
		t.Fatalf("AccountsStats: %v", err)
	}
	if !ok {
		t.Fatal("ok = false with a populated rollup — the hub would 503 on good data")
	}

	for _, c := range []struct {
		name string
		got  int64
		want int64
	}{
		{"TotalAccounts", got.TotalAccounts, 53_800_000},
		{"TotalTrustlines", got.TotalTrustlines, 9_100_000},
		{"TrustlineHoldingAccounts", got.TrustlineHoldingAccounts, 2_400_000},
		{"XLMTotalStroops", got.XLMTotalStroops, 1_050_000_000_000_000_000},
		{"AvgStroops", got.AvgStroops, 19_516_728},
		{"MedianStroops", got.MedianStroops, 15_000_000},
		{"P90Stroops", got.P90Stroops, 310_000_000},
		{"P99Stroops", got.P99Stroops, 9_900_000_000},
		{"Top100XLMStroops", got.Top100XLMStroops, 420_000_000_000_000},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d (a metric-name drift leaves the field at zero, silently)", c.name, c.got, c.want)
		}
	}

	// ComputedAt is the NEWEST timestamp across the metric rows — the swap is
	// atomic, but reporting the oldest would understate freshness.
	if !got.ComputedAt.Equal(newer) {
		t.Errorf("ComputedAt = %s, want the newest metric timestamp %s", got.ComputedAt, newer)
	}

	if len(got.WealthHistogram) != 2 || got.WealthHistogram[0].Bucket != -1 ||
		got.WealthHistogram[0].Accounts != 30_000_000 || got.WealthHistogram[0].XLMStroops != 1_000 {
		t.Errorf("WealthHistogram = %+v, want the two fixture buckets verbatim", got.WealthHistogram)
	}
	if len(got.TrustlineHistogram) != 2 || got.TrustlineHistogram[0].Bucket != "1" ||
		got.TrustlineHistogram[1].Accounts != 700_000 {
		t.Errorf("TrustlineHistogram = %+v, want the two fixture bands verbatim", got.TrustlineHistogram)
	}
	if len(got.TopHeldAssets) != 2 || got.TopHeldAssets[0].Asset != "USDC-"+testIssuer ||
		got.TopHeldAssets[1].Holders != 53_000_000 {
		t.Errorf("TopHeldAssets = %+v, want the two fixture rows in query order", got.TopHeldAssets)
	}
}

// TestAccountsStats_UnprovisionedRollupIsNotAnError — ok=false is the
// "warming" signal the handler turns into a 503. Returning a zero-valued
// snapshot with ok=true instead would publish "0 accounts on the Stellar
// network" as a measured fact.
func TestAccountsStats_UnprovisionedRollupIsNotAnError(t *testing.T) {
	// An EMPTY probe result: the table exists but the rollup has never run.
	conn := &stubConn{respond: func(q string) (driver.Rows, error) {
		if isStatsProbe(q) {
			return &stubRows{}, nil
		}
		t.Fatalf("no read may run before the probe settles; got: %s", q)
		return nil, nil
	}}
	r := &ExplorerReader{conn: conn}

	got, ok, err := r.AccountsStats(t.Context())
	if err != nil {
		t.Fatalf("AccountsStats(empty rollup) = %v, want a nil-error not-ready", err)
	}
	if ok {
		t.Fatal("ok = true on an empty rollup — zeros would be served as facts")
	}
	if !reflect.DeepEqual(got, AccountsStats{}) {
		t.Errorf("snapshot = %+v, want the zero value", got)
	}
}

// TestAccountsStats_ReadFailurePropagates — a failed sub-read must be an
// ERROR, not a not-ready. ok=false would tell the handler "still warming" for
// what is actually a broken store, hiding the outage behind a warming page.
func TestAccountsStats_ReadFailurePropagates(t *testing.T) {
	boom := errors.New("memory limit exceeded")
	for _, tc := range []struct {
		name  string
		match func(string) bool
	}{
		{"metrics", isStatsMetrics},
		{"wealth histogram", isWealthHistogram},
		{"trustline histogram", isTrustlineHistogram},
		{"top held assets", isTopHeldAssets},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &stubConn{}
			conn.respond = func(q string) (driver.Rows, error) {
				if tc.match(q) {
					return nil, boom
				}
				if isStatsProbe(q) {
					return &stubRows{data: [][]any{{int64(1)}}}, nil
				}
				return &stubRows{}, nil
			}
			r := &ExplorerReader{conn: conn}
			_, ok, err := r.AccountsStats(t.Context())
			if err == nil {
				t.Fatalf("a failing %s read was reported as ok=%v with no error", tc.name, ok)
			}
			if !errors.Is(err, boom) {
				t.Errorf("err = %v, want it to wrap %v", err, boom)
			}
			if ok {
				t.Error("ok = true alongside an error")
			}
		})
	}
}

// TestAccountsStats_TopHeldBoardIsRankedAndBounded pins the two properties of
// the most-held board that are not visible from its result: it is ordered by
// holders DESC (an unordered read returns an arbitrary twelve assets) and it
// binds topHeldAssetsLimit rather than reading the whole counts table.
func TestAccountsStats_TopHeldBoardIsRankedAndBounded(t *testing.T) {
	r, conn := statsConn(t, nil, nil, nil, nil)
	if _, _, err := r.AccountsStats(t.Context()); err != nil {
		t.Fatalf("AccountsStats: %v", err)
	}

	var q string
	var args []any
	for i, cand := range conn.queries {
		if isTopHeldAssets(cand) {
			q, args = cand, conn.args[i]
		}
	}
	if q == "" {
		t.Fatal("no asset_holders_counts read was issued")
	}
	if !strings.Contains(q, "ORDER BY holders DESC") {
		t.Errorf("top-held board is not ranked — an unordered LIMIT returns arbitrary assets:\n%s", q)
	}
	if !strings.Contains(q, "LIMIT ?") {
		t.Errorf("top-held board has no bound LIMIT:\n%s", q)
	}
	if len(args) != 1 || args[0] != topHeldAssetsLimit {
		t.Errorf("bound args = %v, want [%d] (topHeldAssetsLimit)", args, topHeldAssetsLimit)
	}
}

// TestAccountsStats_HistogramsAreOrderedByBucket — both histograms are
// rendered as ordered bars; an unordered read shuffles the buckets and the
// chart becomes nonsense without ever erroring.
func TestAccountsStats_HistogramsAreOrderedByBucket(t *testing.T) {
	r, conn := statsConn(t, nil, nil, nil, nil)
	if _, _, err := r.AccountsStats(t.Context()); err != nil {
		t.Fatalf("AccountsStats: %v", err)
	}
	for _, tc := range []struct {
		name  string
		match func(string) bool
	}{
		{"wealth histogram", isWealthHistogram},
		{"trustline histogram", isTrustlineHistogram},
	} {
		found := false
		for _, q := range conn.queries {
			if !tc.match(q) {
				continue
			}
			found = true
			if !strings.Contains(q, "ORDER BY bucket") {
				t.Errorf("%s is not ordered by bucket:\n%s", tc.name, q)
			}
		}
		if !found {
			t.Errorf("no %s read was issued", tc.name)
		}
	}
}

// TestAccountsStats_ProbeGatesEveryRead — the probe exists so a deployment
// without the rollup tables does not issue four failing reads per request.
// Exactly five queries: the probe plus one read per table.
func TestAccountsStats_ProbeGatesEveryRead(t *testing.T) {
	r, conn := statsConn(t, nil, nil, nil, nil)
	if _, _, err := r.AccountsStats(t.Context()); err != nil {
		t.Fatalf("AccountsStats: %v", err)
	}
	if len(conn.queries) != 5 {
		t.Fatalf("issued %d queries, want 5 (probe + four rollup reads):\n%s",
			len(conn.queries), strings.Join(conn.queries, "\n---\n"))
	}
	if !isStatsProbe(conn.queries[0]) {
		t.Errorf("first query is not the schema probe:\n%s", conn.queries[0])
	}
}
