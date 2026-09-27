package clickhouse

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"testing"

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

// TestAccountsByWealth_CarriesExactNativeStroops pins CA2-A03-correct-5 at
// the reader: the native balance reaches the caller as an integer stroop
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
