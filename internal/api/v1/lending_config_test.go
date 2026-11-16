package v1_test

import (
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// configuredPool is a pool instance, one reserve with a config whose supply cap is above 2^63 and one with no config.
func configuredPool(t *testing.T, pool string) *stubExplorerReader {
	t.Helper()
	inst := &clickhouse.PoolInstance{
		Config: blend.PoolConfig{Oracle: mkCStrkey(t, 9), MinCollateral: bigInt(t, "18446744073709551621"), BstopRate: 2_000_000, Status: 1, MaxPositions: 6},
		Admin:  "GDEA2WMOWYQ3T56A2PZH5QYYEQBAORW2AUUJYX4FFD5GDPESAICTFTAL",
	}
	withCfg := reserveState(pool, mkCStrkey(t, 3), 7, true, big.NewInt(100), big.NewInt(50))
	withCfg.Config = &blend.ReserveConfig{
		Index: 2, Decimals: 7, CFactor: 9_500_000, LFactor: 9_000_000, Util: 8_000_000, MaxUtil: 9_500_000,
		RBase: 300_000, ROne: 400_000, RTwo: 1_200_000, RThree: 50_000_000, Reactivity: 20,
		SupplyCap: bigInt(t, "18446744073709551623"), Enabled: true,
	}
	withCfg.Data = blend.ReserveData{IRMod: big.NewInt(5_169_744), BackstopCredit: bigInt(t, "9223372036854775817"), LastTime: 1_700_000_000, Version: blend.PoolV2}
	noCfg := reserveState(pool, mkCStrkey(t, 4), 7, false, big.NewInt(10), big.NewInt(0))
	return &stubExplorerReader{poolInstance: inst, reserves: []clickhouse.BlendReserveState{withCfg, noCfg}}
}

// TestLendingPoolReserves_ServesReserveAndPoolConfig — config fields are raw contract integers as exact strings, a reserve
// with no config serves config null, and the pool config carries the labelled status.
func TestLendingPoolReserves_ServesReserveAndPoolConfig(t *testing.T) {
	pool := mkCStrkey(t, 8)
	srv := v1.New(v1.Options{
		Explorer: configuredPool(t, pool),
		Lending:  &stubLendingReader{assets: []string{mkCStrkey(t, 3), mkCStrkey(t, 4)}},
	})
	resp := mustGet(t, httpTestServer(t, srv).URL+"/v1/lending/pools/"+pool+"/reserves")
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body struct{ Data v1.LendingPoolReservesView }
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	got := body.Data
	if len(got.Reserves) != 2 || got.ConfigDecimals != 7 {
		t.Fatalf("view = %+v", got)
	}
	c := got.Reserves[0].Config
	if c == nil || strOrNil(c.SupplyCap) != "18446744073709551623" || c.CollateralFactor != "9500000" || c.LiabilityFactor != "9000000" ||
		c.TargetUtilization != "8000000" || c.MaxUtilization != "9500000" || c.RBase != "300000" || c.ROne != "400000" ||
		c.RTwo != "1200000" || c.RThree != "50000000" || c.Reactivity != "20" || c.Index != 2 || !c.Enabled {
		t.Errorf("config = %+v", c)
	}
	r := got.Reserves[0]
	if strOrNil(r.IRMod) != "5169744" || r.IRModDecimals != 7 || strOrNil(r.BackstopCredit) != "9223372036854775817" ||
		r.LastUpdate == nil || time.Time(*r.LastUpdate).Unix() != 1_700_000_000 {
		t.Errorf("reserve state = %+v", r)
	}
	if got.Reserves[1].Config != nil || !strings.Contains(string(raw), `"config":null`) {
		t.Errorf("unconfigured reserve config = %+v, want null", got.Reserves[1].Config)
	}
	pc := got.PoolConfig
	if pc == nil || strOrNil(pc.BackstopTakeRate) != "2000000" || strOrNil(pc.MinCollateral) != "18446744073709551621" ||
		pc.Status == nil || *pc.Status != 1 || strOrNil(pc.StatusLabel) != "active" || pc.MaxPositions == nil || *pc.MaxPositions != 6 ||
		strOrNil(pc.Admin) != "GDEA2WMOWYQ3T56A2PZH5QYYEQBAORW2AUUJYX4FFD5GDPESAICTFTAL" || strOrNil(pc.Oracle) != mkCStrkey(t, 9) {
		t.Errorf("pool config = %+v", pc)
	}
}

// TestLendingPoolReserves_V1PoolServesStatusUnlabelled — V1 status codes are served as the integer only, and its ir_mod is 9-decimal.
func TestLendingPoolReserves_V1PoolServesStatusUnlabelled(t *testing.T) {
	pool := mkCStrkey(t, 8)
	explorer := configuredPool(t, pool)
	explorer.reserves[0].Data.Version = blend.PoolV1
	v1Version := blend.PoolV1
	srv := v1.New(v1.Options{
		Explorer: explorer,
		Lending:  &stubLendingReader{assets: []string{mkCStrkey(t, 3)}, version: &v1Version},
	})
	got := getReserves(t, srv, pool)
	if got.PoolConfig == nil || got.PoolConfig.Status == nil || got.PoolConfig.StatusLabel != nil || got.Reserves[0].IRModDecimals != 9 {
		t.Fatalf("pool config = %+v reserve = %+v", got.PoolConfig, got.Reserves[0])
	}
}

// TestLendingPoolReserves_ServesPoolConfigWithoutReserves — a pool with no reserve state still serves its pool config; a pool in
// setup status has exactly this shape.
func TestLendingPoolReserves_ServesPoolConfigWithoutReserves(t *testing.T) {
	pool := mkCStrkey(t, 8)
	inst := &clickhouse.PoolInstance{Config: blend.PoolConfig{Oracle: mkCStrkey(t, 9), BstopRate: 1_000_000, Status: 6, MaxPositions: 4}}
	srv := v1.New(v1.Options{
		Explorer: &stubExplorerReader{poolInstance: inst},
		Lending:  &stubLendingReader{},
	})
	got := getReserves(t, srv, pool)
	pc := got.PoolConfig
	if got.Reserves == nil || len(got.Reserves) != 0 || pc == nil || pc.Status == nil || *pc.Status != 6 ||
		strOrNil(pc.StatusLabel) != "setup" || pc.MinCollateral != nil {
		t.Fatalf("reserves = %v pool config = %+v", got.Reserves, pc)
	}
}

type stubConfigHistoryReader struct {
	stubLendingReader
	rows      []timescale.LendingConfigChange
	gotLimit  int
	gotCursor timescale.LendingConfigCursor
}

// LendingConfigHistory serves the rows after the one the cursor names exactly; a cursor naming no row serves nothing.
func (r *stubConfigHistoryReader) LendingConfigHistory(_ context.Context, _ string, limit int, cur timescale.LendingConfigCursor) ([]timescale.LendingConfigChange, error) {
	r.gotLimit, r.gotCursor = limit, cur
	rows := r.rows
	if cur.IsSet() {
		rows = nil
		for i, c := range r.rows {
			if c.Ts.Equal(cur.Ts) && c.Ledger == cur.Ledger && c.TxHash == cur.TxHash && c.OpIndex == cur.OpIndex &&
				c.EventIndex == cur.EventIndex && c.Kind == cur.Kind {
				rows = r.rows[i+1:]
				break
			}
		}
	}
	return rows[:min(limit, len(rows))], nil
}

const configMeta = `{"index":0,"decimals":7,"c_factor":9000000,"l_factor":9500000,"util":8000000,"max_util":9500000,` +
	`"r_base":100000,"r_one":500000,"r_two":1500000,"r_three":30000000,"reactivity":20,"supply_cap":"18446744073709551623","enabled":true}`

func historyRows(t *testing.T) []timescale.LendingConfigChange {
	t.Helper()
	asset := mkCStrkey(t, 3)
	ts := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	row := func(i int, kind, asset, attrs, meta string, applied bool) timescale.LendingConfigChange {
		c := timescale.LendingConfigChange{
			Ts: ts.Add(-time.Duration(i) * time.Minute), Ledger: uint32(100 - i), TxHash: strings.Repeat(string(rune('a'+i)), 64),
			OpIndex: uint32(10 + i), EventIndex: uint32(20 + i), Kind: kind, Asset: asset, Attributes: []byte(attrs), Applied: applied,
		}
		if meta != "" {
			c.Metadata = []byte(meta)
		}
		return c
	}
	return []timescale.LendingConfigChange{
		row(0, "set_status", "", `{"status":3,"by_admin":true}`, "", false),
		row(1, "update_pool", "", `{"backstop_take_rate":1500000,"max_positions":4,"min_collateral":"18446744073709551621"}`, "", false),
		row(2, "set_reserve", asset, `{"index":0}`, configMeta, false),
		row(3, "queue_set_reserve", asset, `{}`, configMeta, true),
		row(4, "cancel_set_reserve", asset, `{}`, "", false),
		row(5, "queue_set_reserve", asset, `{}`, configMeta, false),
	}
}

func getConfigHistory(t *testing.T, srv *v1.Server, query string) (int, v1.LendingConfigHistoryView, *v1.Pagination) {
	t.Helper()
	resp := mustGet(t, httpTestServer(t, srv).URL+"/v1/lending/pools/"+mkCStrkey(t, 8)+"/config-history"+query)
	var body struct {
		Data       v1.LendingConfigHistoryView
		Pagination *v1.Pagination
	}
	if resp.StatusCode == http.StatusOK {
		mustDecode(t, resp, &body)
	}
	return resp.StatusCode, body.Data, body.Pagination
}

// TestLendingConfigHistory_MapsEveryKind — reserve steps carry the decoded config (supply cap exact above 2^63) and the
// queue's applied verdict; pool-level events carry only the fields they set.
func TestLendingConfigHistory_MapsEveryKind(t *testing.T) {
	stub := &stubConfigHistoryReader{rows: historyRows(t)}
	code, got, next := getConfigHistory(t, v1.New(v1.Options{Lending: stub}), "")
	if code != http.StatusOK || len(got.Changes) != 6 || next != nil || got.ConfigDecimals != 7 || stub.gotLimit != 51 {
		t.Fatalf("code=%d changes=%d next=%v limit=%d", code, len(got.Changes), next, stub.gotLimit)
	}
	c := got.Changes
	if pc := c[0].PoolConfig; c[0].Asset != nil || pc == nil || pc.Status == nil || *pc.Status != 3 || strOrNil(pc.StatusLabel) != "on_ice" ||
		pc.BackstopTakeRate != nil {
		t.Errorf("set_status = %+v pool_config = %+v", c[0], pc)
	}
	if pc := c[1].PoolConfig; pc == nil || strOrNil(pc.BackstopTakeRate) != "1500000" || pc.MaxPositions == nil || *pc.MaxPositions != 4 ||
		strOrNil(pc.MinCollateral) != "18446744073709551621" || pc.Status != nil {
		t.Errorf("update_pool pool_config = %+v", pc)
	}
	if c[2].Config == nil || strOrNil(c[2].Config.SupplyCap) != "18446744073709551623" || c[2].Config.CollateralFactor != "9000000" || c[2].Applied != nil {
		t.Errorf("set_reserve = %+v", c[2])
	}
	if c[3].Applied == nil || !*c[3].Applied || c[5].Applied == nil || *c[5].Applied || c[4].Config != nil || strOrNil(c[4].Asset) != mkCStrkey(t, 3) {
		t.Errorf("queue/cancel = %+v %+v %+v", c[3], c[4], c[5])
	}
}

// TestLendingConfigHistory_PaginatesWithAKeysetCursor — a full page returns the last row's position, which round-trips.
func TestLendingConfigHistory_PaginatesWithAKeysetCursor(t *testing.T) {
	rows := historyRows(t)
	stub := &stubConfigHistoryReader{rows: rows}
	srv := v1.New(v1.Options{Lending: stub})
	code, got, next := getConfigHistory(t, srv, "?limit=2")
	if code != http.StatusOK || len(got.Changes) != 2 || next == nil || next.Next == "" {
		t.Fatalf("code=%d changes=%d next=%v", code, len(got.Changes), next)
	}
	code, got, _ = getConfigHistory(t, srv, "?limit=2&cursor="+next.Next)
	if code != http.StatusOK || len(got.Changes) != 2 || got.Changes[0].Kind != rows[2].Kind || got.Changes[1].Kind != rows[3].Kind {
		t.Fatalf("cursor page status = %d changes = %+v, want rows 2 and 3", code, got.Changes)
	}
	want := timescale.LendingConfigCursor{
		Ts: rows[1].Ts, Ledger: rows[1].Ledger, TxHash: rows[1].TxHash, OpIndex: 11, EventIndex: 21, Kind: "update_pool",
	}
	if !stub.gotCursor.Ts.Equal(want.Ts) || stub.gotCursor.Ledger != want.Ledger || stub.gotCursor.TxHash != want.TxHash ||
		stub.gotCursor.OpIndex != want.OpIndex || stub.gotCursor.EventIndex != want.EventIndex || stub.gotCursor.Kind != want.Kind {
		t.Errorf("cursor = %+v, want %+v", stub.gotCursor, want)
	}
	tx := strings.Repeat("b", 64)
	for _, q := range []string{
		"?cursor=junk", "?cursor=1.2.abc.3.4.set_reserve", "?cursor=1.2." + tx + ".3.4", "?cursor=1.2." + tx + ".3.4.transfer",
		"?limit=0", "?limit=201",
	} {
		if code, _, _ := getConfigHistory(t, srv, q); code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, code)
		}
	}
}

// TestLendingConfigHistory_EmptyWithoutAHistoryReader — a reader without the history read serves an empty list.
func TestLendingConfigHistory_EmptyWithoutAHistoryReader(t *testing.T) {
	code, got, _ := getConfigHistory(t, v1.New(v1.Options{Lending: &stubLendingReader{}}), "")
	if code != http.StatusOK || got.Changes == nil || len(got.Changes) != 0 {
		t.Fatalf("code=%d changes=%v", code, got.Changes)
	}
}
