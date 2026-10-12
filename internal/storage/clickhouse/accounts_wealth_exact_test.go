package clickhouse

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// wealthExactFakeRows returns one ranking row whose native balance is past
// 2^53 stroops, where the float64 ranking key has already lost the stroop.
type wealthExactFakeRows struct {
	driver.Rows
	i int
}

const wealthExactStroops = 100_000_000_001_234_567 // 10,000,000,000.1234567 XLM

func (r *wealthExactFakeRows) Next() bool {
	r.i++
	return r.i == 1
}

func (r *wealthExactFakeRows) Scan(dest ...any) error {
	*dest[0].(*string) = "GEXACT"
	*dest[1].(*string) = "100000000001234567.000000000000000000"
	*dest[2].(*big.Int) = *big.NewInt(wealthExactStroops)
	return nil
}

func (r *wealthExactFakeRows) Err() error   { return nil }
func (r *wealthExactFakeRows) Close() error { return nil }

type wealthExactFakeConn struct{ driver.Conn }

func (wealthExactFakeConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	return &wealthExactFakeRows{}, nil
}

// TestAccountsByWealth_CarriesExactNativeStroops pins at
// the reader that the native balance reaches the caller as an integer stroop
// count, not only as the float ranking key.
func TestAccountsByWealth_CarriesExactNativeStroops(t *testing.T) {
	t.Parallel()
	r := &ExplorerReader{conn: wealthExactFakeConn{}}
	rows, err := r.AccountsByWealth(context.Background(), []string{"native"}, []string{"1"}, 5)
	if err != nil || len(rows) != 1 {
		t.Fatalf("AccountsByWealth = %d rows, err %v; want 1 row", len(rows), err)
	}
	if got := rows[0].NativeStroops.String(); got != "100000000001234567" {
		t.Errorf("NativeStroops = %s, want the exact 100000000001234567", got)
	}
	if got := rows[0].Value.FloatString(7); got != "10000000000.1234567" {
		t.Errorf("Value = %s, want the exact 10000000000.1234567", got)
	}
}

// The served wealth figure is the query's sum, so the sum must not be a float,
// and the scale wealthPriceArgs renders to must be the one the query casts to.
func TestAccountsByWealthQuery_ExactDecimalSum(t *testing.T) {
	t.Parallel()
	if strings.Contains(accountsByWealthQuery, "Float") {
		t.Errorf("accountsByWealthQuery sums in a float type:\n%s", accountsByWealthQuery)
	}
	if cast := fmt.Sprintf("toDecimal256(p, %d)", wealthPriceScale); !strings.Contains(accountsByWealthQuery, cast) {
		t.Errorf("accountsByWealthQuery does not cast prices with %q:\n%s", cast, accountsByWealthQuery)
	}
}

func TestWealthPriceArgs(t *testing.T) {
	t.Parallel()
	got, err := wealthPriceArgs([]string{"1", "0.333333333333333333", "1e-3"})
	if err != nil {
		t.Fatalf("wealthPriceArgs: %v", err)
	}
	want := []string{"1.000000000000000000", "0.333333333333333333", "0.001000000000000000"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("price[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	for _, bad := range []string{"abc", "0", "-1", ""} {
		if _, err := wealthPriceArgs([]string{"1", bad}); err == nil {
			t.Errorf("wealthPriceArgs accepted %q", bad)
		}
	}
}

// TestAccountsByWealthQuery_Bounds — the wealth refresh additionally carries
// the 150s execution ceiling (its Go-side budget is 3 min; the connection
// default of 30s killed it silently — see accountsByWealthQuery's doc).
func TestAccountsByWealthQuery_Bounds(t *testing.T) {
	requireScanSettings(t, "accountsByWealthQuery", accountsByWealthQuery)
	if !strings.Contains(accountsByWealthQuery, "max_execution_time = 150") {
		t.Errorf("accountsByWealthQuery missing its 150s execution ceiling:\n%s", accountsByWealthQuery)
	}
}

// TestWealthCacheServesAnyLimitFromOneEntry pins that the cache stores one ranking every request size slices
// from. Per-limit keys would let prewarm warm only limit=100 while every other limit misses and kicks its
// own slow scan.
func TestWealthCacheServesAnyLimitFromOneEntry(t *testing.T) {
	t.Parallel()
	c := newAccountsWealthCache()

	ranking := make([]AccountWealth, accountsWealthMaxLimit)
	for i := range ranking {
		ranking[i] = AccountWealth{AccountID: "acct", Value: big.NewRat(int64(accountsWealthMaxLimit-i), 1)}
	}
	c.put(AccountWealthSnapshot{Rows: ranking, Basis: WealthBasisUSD, AsOf: time.Now()})

	snap, ok := c.get()
	rows := snap.Rows
	if !ok {
		t.Fatal("get after put returned miss")
	}
	if len(rows) != accountsWealthMaxLimit {
		t.Fatalf("cached ranking has %d rows, want %d", len(rows), accountsWealthMaxLimit)
	}

	// Every realistic request size slices cleanly from the single entry.
	for _, limit := range []int{5, 10, 25, 100, 199, 500} {
		got := clampWealth(rows, limit)
		want := limit
		if want > len(rows) {
			want = len(rows)
		}
		if len(got) != want {
			t.Errorf("clampWealth(limit=%d) = %d rows, want %d", limit, len(got), want)
		}
		if len(got) > 0 && got[0].Value.Cmp(got[len(got)-1].Value) < 0 {
			t.Errorf("limit=%d: slice not still descending", limit)
		}
	}
}

// TestWealthCacheStaleServing pins that an EXPIRED entry is still returned with its real timestamp, so
// callers can serve degraded-but-honest instead of 503; the cache never withholds a filled entry.
func TestWealthCacheStaleServing(t *testing.T) {
	t.Parallel()
	c := newAccountsWealthCache()
	staleAt := time.Now().Add(-2 * AccountsWealthCacheTTL)
	c.put(AccountWealthSnapshot{Rows: []AccountWealth{{AccountID: "a", Value: big.NewRat(1, 1)}}, Basis: WealthBasisUSD, AsOf: staleAt})
	snap, ok := c.get()
	rows, at := snap.Rows, snap.AsOf
	if !ok {
		t.Fatal("expired entry withheld — stale-serving regressed to a hard miss")
	}
	if len(rows) != 1 || !at.Equal(staleAt) {
		t.Errorf("stale entry mangled: rows=%d at=%v want 1 row at %v", len(rows), at, staleAt)
	}
	if time.Since(at) <= AccountsWealthCacheTTL {
		t.Error("test setup: entry should read as stale to a TTL-comparing caller")
	}
}

// TestWealthCacheNilSafe — a nil cache (zero-value reader in some tests)
// is a permanent miss, never a panic.
func TestWealthCacheNilSafe(t *testing.T) {
	t.Parallel()
	var c *accountsWealthCache
	if _, ok := c.get(); ok {
		t.Error("nil cache reported a hit")
	}
	c.put(AccountWealthSnapshot{Rows: []AccountWealth{{AccountID: "a", Value: big.NewRat(1, 1)}}, Basis: WealthBasisUSD, AsOf: time.Now()}) // must not panic
	if _, owner := c.beginFlight(); owner {
		t.Error("nil cache granted flight ownership")
	}
}

// TestWealthCacheSingleFlight — a second beginFlight while one is in flight
// does not get ownership.
func TestWealthCacheSingleFlight(t *testing.T) {
	t.Parallel()
	c := newAccountsWealthCache()
	ch, owner := c.beginFlight()
	if !owner {
		t.Fatal("first beginFlight did not get ownership")
	}
	if _, owner2 := c.beginFlight(); owner2 {
		t.Error("second concurrent beginFlight also got ownership")
	}
	c.endFlight(ch)
	if _, owner3 := c.beginFlight(); !owner3 {
		t.Error("beginFlight after endFlight did not get ownership")
	}
}
