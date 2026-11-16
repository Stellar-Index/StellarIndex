package clickhouse

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
)

// TestBLNTPoolReserves_RealStorageEntries runs the shared reserve reader over the real lake rows of a BLNT
// pool: the keys it builds must be the exact key_xdr the lake holds, and the supply APR must be BLNT's
// fee-adjusted one, not Blend's.
func TestBLNTPoolReserves_RealStorageEntries(t *testing.T) {
	b, err := os.ReadFile("../../../test/fixtures/blnt_storage/reserve_entries.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Pool, Kind, Asset string
		KeyXDR            string `json:"key_xdr"`
		EntryXDR          string `json:"entry_xdr"`
	}
	if err := json.Unmarshal(b, &fixtures); err != nil {
		t.Fatal(err)
	}
	const usdc = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	var rows [][]any
	var assets []string
	configs := map[string]blnt.ReserveConfig{}
	for _, f := range fixtures {
		if f.Pool != blnt.MainnetFixedPool {
			continue
		}
		switch f.Kind {
		case "ResConfig":
			v, ok := contractDataValue(f.EntryXDR)
			cfg, err := blnt.DecodeReserveConfig(v)
			if !ok || err != nil {
				t.Fatalf("ResConfig %s: %v", f.Asset, err)
			}
			configs[f.Asset] = cfg
			continue
		case "ResData":
			assets = append(assets, f.Asset)
		}
		rows = append(rows, []any{f.KeyXDR, f.EntryXDR})
	}
	conn := &stubConn{respond: func(string) (driver.Rows, error) { return &stubRows{data: rows}, nil }}
	r := &ExplorerReader{conn: conn, ttlVerdicts: fixedTTLCache(nil)}

	states, err := r.BLNTPoolReserves(t.Context(), blnt.MainnetFixedPool, assets, configs)
	if err != nil {
		t.Fatalf("BLNTPoolReserves: %v", err)
	}
	requested, _ := conn.args[0][0].([]string)
	for _, row := range rows {
		if !slices.Contains(requested, row[0].(string)) {
			t.Errorf("lake key %s was not requested; the key builder does not match BLNT storage", row[0])
		}
	}
	if len(states) != 3 {
		t.Fatalf("states = %d, want 3", len(states))
	}
	for _, st := range states {
		if st.Asset != usdc {
			continue
		}
		if !st.DecimalsFound || st.Metrics.SuppliedUnderlying.String() != "1985903398374" ||
			st.Metrics.BorrowedUnderlying.String() != "1599586439842" ||
			st.Metrics.BorrowAPR != 0.0384507 || st.Metrics.SupplyAPR != 0.0247272 {
			t.Errorf("USDC reserve = %+v", st)
		}
		return
	}
	t.Fatal("no USDC reserve state")
}
