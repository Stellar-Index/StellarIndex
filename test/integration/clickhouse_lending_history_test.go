//go:build integration

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestLendingReserveHistory_FinalSeedBucketsAndWatermark reads lending_state_hourly through the reader: FINAL keeps an hour's
// highest version, the seed carries the state in force at the aligned from, a day keeps its last hour, the config in force
// pairs with each point, and the watermark's hour is withheld until the derive passes its end.
func TestLendingReserveHistory_FinalSeedBucketsAndWatermark(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")
	for _, q := range []string{`TRUNCATE TABLE stellar.entry_history_watermark`, `TRUNCATE TABLE stellar.lending_state_hourly`} {
		if err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	reader, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	var poolID, assetID, otherID xdr.ContractId
	poolID[0], assetID[0], otherID[0] = 0x71, 0x72, 0x73
	asset := strkey.MustEncode(strkey.VersionByteContract, assetID[:])
	key := resDataKeyB64(t, poolID, assetID)
	cfgKey := lendingPoolKeyB64(t, poolID, lendingPoolDataKey("ResConfig", assetID))
	instKey := lendingPoolKeyB64(t, poolID, xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance})
	day := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	c := lendingHistoryCase{t: t, ctx: ctx, conn: conn, reader: reader, pool: strkey.MustEncode(strkey.VersionByteContract, poolID[:]), day: day}
	q := chstore.LendingHistoryQuery{From: day.Add(9 * time.Hour), To: day.Add(48 * time.Hour), MaxPoints: 2000}

	if _, err := reader.LendingReserveHistory(ctx, c.pool, asset, q); !errors.Is(err, chstore.ErrLendingHistoryNotReady) {
		t.Fatalf("before any derive: err = %v, want not ready", err)
	}
	const base = 1_000_000_000_000
	data := func(ledger uint32, bRate uint64) string { return resDataEntryB64(t, poolID, assetID, ledger, bRate) }
	c.setWatermark(lhL0+1000, day.Add(34*time.Hour+20*time.Minute))
	c.insert(instKey, lhL0+50, 7*time.Hour, lendingPoolEntryB64(t, poolID, lendingPoolInstanceVal(poolID)))
	c.insert(cfgKey, lhL0+150, 9*time.Hour, lendingPoolEntryB64(t, poolID, lendingResConfigVal(9_000_000)))
	// Later in the same hour as the ledger 300 state: the config in force at that state is unknown.
	c.insert(cfgKey, lhL0+320, 13*time.Hour+50*time.Minute, lendingPoolEntryB64(t, poolID, lendingResConfigVal(8_000_000)))
	c.insert(key, lhL0+100, 8*time.Hour+30*time.Minute, data(lhL0+100, base+1)) // seed
	c.insert(key, lhL0+200, 10*time.Hour+20*time.Minute, data(lhL0+200, base+2))
	c.insert(key, lhL0+190, 10*time.Hour+5*time.Minute, data(lhL0+190, base+99)) // the lower version of the hour, inserted last
	c.insert(key, lhL0+300, 13*time.Hour+10*time.Minute, data(lhL0+300, base+3))
	c.insert(key, lhL0+400, 33*time.Hour, data(lhL0+400, base+4))
	c.insert(key, lhL0+950, 34*time.Hour+5*time.Minute, data(lhL0+950, base+5)) // the watermark's own hour

	hourly := c.expect("hourly, the watermark's hour withheld", asset, q, "[100 200 300 400]", false)
	if b := hourly.Points[1].State.Data.BRate.Uint64(); b != base+2 || hourly.Coverage.ThroughLedger != lhL0+1000 || hourly.Protocol != "blend_v2" {
		t.Fatalf("hour 10:00 b_rate = %d (FINAL must keep ledger 200), coverage %+v", b, hourly.Coverage)
	}
	checkConfigInForce(t, hourly, []lhConfigWant{{0, false}, {lhL0 + 150, false}, {0, true}, {lhL0 + 320, false}})

	// A from inside an hour is aligned to its start: the seed is the state in force at 10:00.
	mid := q
	mid.From = day.Add(10*time.Hour + 10*time.Minute)
	c.expect("from 10:10", asset, mid, "[100 200 300 400]", false)

	// A newer version of the watermark's hour from a derive still in flight changes nothing served.
	c.insert(key, lhL0+1010, 34*time.Hour+40*time.Minute, data(lhL0+1010, base+6))
	c.expect("during a derive", asset, q, "[100 200 300 400]", false)

	q.Daily = true
	c.expect("daily, each day's last hour from the day's start", asset, q, "[300 400]", true)

	q.Daily, q.MaxPoints = false, 2
	if _, err := reader.LendingReserveHistory(ctx, c.pool, asset, q); !errors.Is(err, chstore.ErrLendingHistoryTooManyPoints) {
		t.Fatalf("3 points over a cap of 2: err = %v", err)
	}
	q.From, q.MaxPoints = day.Add(-24*time.Hour), 2000
	c.expect("from before the first state", asset, q, "[100 200 300 400]", true)
	c.expect("window before the first state", asset, chstore.LendingHistoryQuery{From: day, To: day.Add(5 * time.Hour), MaxPoints: 2000}, "[]", true)
	other := strkey.MustEncode(strkey.VersionByteContract, otherID[:])
	if _, err := reader.LendingReserveHistory(ctx, c.pool, other, q); !errors.Is(err, chstore.ErrLendingReserveNotFound) {
		t.Fatalf("asset with no recorded state: err = %v, want not found", err)
	}

	// Once the derive passes the end of hour 34, that hour's final version is served.
	c.setWatermark(lhL0+1100, day.Add(35*time.Hour+10*time.Minute))
	c.expect("after the derive", asset, q, "[100 200 300 400 1010]", true)
}

// lhL0 offsets the test's ledgers clear of other tests' stellar.ledgers rows.
const lhL0 = 88_100_000

type lendingHistoryCase struct {
	t      *testing.T
	ctx    context.Context
	conn   driver.Conn
	reader *chstore.ExplorerReader
	pool   string
	day    time.Time
}

// setWatermark records the derive as complete through ledger, closing at at.
func (c lendingHistoryCase) setWatermark(ledger uint32, at time.Time) {
	c.t.Helper()
	if err := c.conn.Exec(c.ctx, `INSERT INTO stellar.ledgers (ledger_seq, close_time, ledger_hash, prev_hash, protocol_version)
		VALUES (?, ?, ?, '00', 23)`, ledger, at, fmt.Sprintf("%064d", ledger)); err != nil {
		c.t.Fatal(err)
	}
	if err := c.conn.Exec(c.ctx, `INSERT INTO stellar.entry_history_watermark (name, thru_ledger) VALUES ('lending_state', ?)`, ledger); err != nil {
		c.t.Fatal(err)
	}
}

// insert writes one lending_state_hourly row of the pool, closing at day+at.
func (c lendingHistoryCase) insert(key string, ledger uint32, at time.Duration, entry string) {
	c.t.Helper()
	ts := c.day.Add(at)
	if err := c.conn.Exec(c.ctx, `INSERT INTO stellar.lending_state_hourly
		(contract_id, protocol, key_xdr, hour, ledger_seq, intra_ledger_seq, close_time, entry_xdr)
		VALUES (?, 'blend_v2', ?, toStartOfHour(toDateTime(?, 'UTC')), ?, 1, toDateTime(?, 'UTC'), ?)`,
		c.pool, key, ts.Unix(), ledger, ts.Unix(), entry); err != nil {
		c.t.Fatal(err)
	}
}

// expect reads the reserve history and requires its point ledgers (offset by lhL0) and before_first_state.
func (c lendingHistoryCase) expect(label, asset string, q chstore.LendingHistoryQuery, want string, before bool) chstore.LendingReserveHistory {
	c.t.Helper()
	h, err := c.reader.LendingReserveHistory(c.ctx, c.pool, asset, q)
	if err != nil {
		c.t.Fatalf("%s: %v", label, err)
	}
	got := make([]uint32, 0, len(h.Points))
	for _, p := range h.Points {
		got = append(got, p.StateLedger-lhL0)
	}
	if fmt.Sprint(got) != want || h.BeforeFirstState != before {
		c.t.Fatalf("%s: points %v (before_first_state %v), want %s (%v)", label, got, h.BeforeFirstState, want, before)
	}
	return h
}

type lhConfigWant struct {
	ledger    uint32
	uncertain bool
}

func checkConfigInForce(t *testing.T, h chstore.LendingReserveHistory, want []lhConfigWant) {
	t.Helper()
	for i, w := range want {
		p := h.Points[i]
		if p.ConfigLedger != w.ledger || p.ConfigUncertain != w.uncertain || p.State.Metrics.HasAPR != (w.ledger != 0) {
			t.Fatalf("point %d: config ledger %d uncertain %v apr %v, want %+v", i, p.ConfigLedger, p.ConfigUncertain, p.State.Metrics.HasAPR, w)
		}
	}
}

// lendingPoolDataKey is a Blend PoolDataKey::<name>(asset) key: Vec[Symbol(name), Address(asset)].
func lendingPoolDataKey(name string, asset xdr.ContractId) xdr.ScVal {
	sym := xdr.ScSymbol(name)
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &asset}
	vec := &xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &sym}, {Type: xdr.ScValTypeScvAddress, Address: &addr}}
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vec}
}

func lendingPoolKeyB64(t *testing.T, pool xdr.ContractId, key xdr.ScVal) string {
	t.Helper()
	b64, err := xdr.MarshalBase64(xdr.LedgerKey{Type: xdr.LedgerEntryTypeContractData, ContractData: &xdr.LedgerKeyContractData{
		Contract: xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &pool}, Key: key, Durability: xdr.ContractDataDurabilityPersistent,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return b64
}

// lendingPoolEntryB64 is a contract_data entry of pool holding val; the reader decodes only the value.
func lendingPoolEntryB64(t *testing.T, pool xdr.ContractId, val xdr.ScVal) string {
	t.Helper()
	b64, err := xdr.MarshalBase64(xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeContractData, ContractData: &xdr.ContractDataEntry{
		Contract: xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &pool},
		Key:      xdr.ScVal{Type: xdr.ScValTypeScvVoid}, Durability: xdr.ContractDataDurabilityPersistent, Val: val,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return b64
}

func lendingSymMap(entries map[string]xdr.ScVal, order ...string) xdr.ScVal {
	m := make(xdr.ScMap, 0, len(order))
	for _, k := range order {
		sym := xdr.ScSymbol(k)
		m = append(m, xdr.ScMapEntry{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}, Val: entries[k]})
	}
	mp := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
}

func lendingU32(n uint32) xdr.ScVal {
	v := xdr.Uint32(n)
	return xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &v}
}

// lendingResConfigVal is a V2 ResConfig value with collateral factor cFactor.
func lendingResConfigVal(cFactor uint32) xdr.ScVal {
	enabled := true
	return lendingSymMap(map[string]xdr.ScVal{
		"c_factor": lendingU32(cFactor), "decimals": lendingU32(7), "enabled": {Type: xdr.ScValTypeScvBool, B: &enabled},
		"index": lendingU32(0), "l_factor": lendingU32(9_500_000), "max_util": lendingU32(9_500_000), "r_base": lendingU32(100_000),
		"r_one": lendingU32(500_000), "r_three": lendingU32(50_000_000), "r_two": lendingU32(5_000_000), "reactivity": lendingU32(20),
		"supply_cap": {Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Lo: 1_000_000_000}}, "util": lendingU32(8_000_000),
	}, "c_factor", "decimals", "enabled", "index", "l_factor", "max_util", "r_base", "r_one", "r_three", "r_two", "reactivity", "supply_cap", "util")
}

// lendingPoolInstanceVal is a pool contract instance whose Config is a V2 PoolConfig.
func lendingPoolInstanceVal(oracle xdr.ContractId) xdr.ScVal {
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &oracle}
	cfg := lendingSymMap(map[string]xdr.ScVal{
		"bstop_rate": lendingU32(2_000_000), "max_positions": lendingU32(8),
		"min_collateral": {Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Lo: 5}},
		"oracle":         {Type: xdr.ScValTypeScvAddress, Address: &addr}, "status": lendingU32(0),
	}, "bstop_rate", "max_positions", "min_collateral", "oracle", "status")
	cfgSym := xdr.ScSymbol("Config")
	storage := xdr.ScMap{{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &cfgSym}, Val: cfg}}
	return xdr.ScVal{Type: xdr.ScValTypeScvContractInstance, Instance: &xdr.ScContractInstance{
		Executable: xdr.ContractExecutable{Type: xdr.ContractExecutableTypeContractExecutableStellarAsset},
		Storage:    &storage,
	}}
}
