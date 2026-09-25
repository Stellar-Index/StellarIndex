package clickhouse

import (
	"context"
	"math/big"
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
	*dest[1].(*float64) = float64(wealthExactStroops) / 1e7
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
	rows, err := r.AccountsByWealth(context.Background(), []string{"native"}, []float64{1}, 5)
	if err != nil || len(rows) != 1 {
		t.Fatalf("AccountsByWealth = %d rows, err %v; want 1 row", len(rows), err)
	}
	if got := rows[0].NativeStroops.String(); got != "100000000001234567" {
		t.Errorf("NativeStroops = %s, want the exact 100000000001234567", got)
	}
}
