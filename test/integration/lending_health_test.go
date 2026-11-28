//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend/oracleagg"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func lhContract(t *testing.T, s string) xdr.ContractId {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, s)
	if err != nil {
		t.Fatal(err)
	}
	var id xdr.ContractId
	copy(id[:], raw)
	return id
}

func lhVec(vals ...xdr.ScVal) xdr.ScVal {
	v := xdr.ScVec(vals)
	vp := &v
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vp}
}

// lhPositions is a Positions entry: collateral and liabilities by ResList index (0 = XLM, 1 = USDC), no plain supply.
func lhPositions(coll, liab map[uint32]uint64) xdr.ScVal {
	return lendingSymMap(map[string]xdr.ScVal{
		"collateral": positionsIndexMap(coll), "liabilities": positionsIndexMap(liab), "supply": positionsIndexMap(nil),
	}, "collateral", "liabilities", "supply")
}

// lhReserve is a ResData with both rates at rate (1e12 is 1.0 on V2, 1e9 on V1).
func lhReserve(t *testing.T, rate string, bSupply, dSupply uint64, lastTime int64) xdr.ScVal {
	lt := xdr.Uint64(lastTime) //nolint:gosec // a ledger-era unix second
	return lendingSymMap(map[string]xdr.ScVal{
		"b_rate": liI128(t, rate), "b_supply": positionsI128(bSupply), "backstop_credit": positionsI128(0),
		"d_rate": liI128(t, rate), "d_supply": positionsI128(dSupply), "ir_mod": positionsI128(10_000_000),
		"last_time": {Type: xdr.ScValTypeScvU64, U64: &lt},
	}, "b_rate", "b_supply", "backstop_credit", "d_rate", "d_supply", "ir_mod", "last_time")
}

// lhResConfig is an applied ResConfig entry: decimals 7 and the given factors.
func lhResConfig(index, cFactor, lFactor uint32) xdr.ScVal {
	on := true
	return lendingSymMap(map[string]xdr.ScVal{
		"c_factor": lendingU32(cFactor), "decimals": lendingU32(7), "enabled": {Type: xdr.ScValTypeScvBool, B: &on},
		"index": lendingU32(index), "l_factor": lendingU32(lFactor), "max_util": lendingU32(9_500_000),
		"r_base": lendingU32(100_000), "r_one": lendingU32(500_000), "r_three": lendingU32(15_000_000),
		"r_two": lendingU32(5_000_000), "reactivity": lendingU32(100), "supply_cap": positionsI128(1 << 62),
		"util": lendingU32(5_000_000),
	}, "c_factor", "decimals", "enabled", "index", "l_factor", "max_util", "r_base", "r_one", "r_three", "r_two",
		"reactivity", "supply_cap", "util")
}

func lhInstance(oracle, backstop xdr.ContractId) xdr.ScVal {
	inst := lendingPoolInstanceVal(oracle)
	sym := xdr.ScSymbol("Backstop")
	*inst.Instance.Storage = append(*inst.Instance.Storage, xdr.ScMapEntry{
		Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}, Val: positionsContractVal(backstop),
	})
	return inst
}

type lhEntry struct {
	key, val xdr.ScVal
	ledger   uint32
}

type lhAccount struct {
	id         xdr.ContractId
	coll, liab map[uint32]uint64
	ledger     uint32
}

// lhPool is one pool's lake fixture: instance, ResList, XLM and USDC reserves whose totals are the positions' sums (so the
// read reconciles), their ResConfig entries, the accounts' Positions and any extra entries.
type lhPool struct {
	id            xdr.ContractId
	rate          string
	lastTime      int64
	cfgXLM        xdr.ScVal
	cfgXLMLedger  uint32
	accounts      []lhAccount
	extra         []lhEntry
	reserveLedger uint32
}

func (p lhPool) entries(t *testing.T, oracle, backstop xdr.ContractId, ledger uint32) []lhEntry {
	t.Helper()
	var sums [2][2]uint64 // [reserve][b, d]
	out := []lhEntry{
		{xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance}, lhInstance(oracle, backstop), ledger - 90},
		{positionsSym("ResList"), lhVec(positionsContractVal(lhContract(t, loXLMSAC)), positionsContractVal(lhContract(t, loUSDCSAC))), ledger - 90},
		{lhVec(positionsSym("ResConfig"), positionsContractVal(lhContract(t, loXLMSAC))), p.cfgXLM, p.cfgXLMLedger},
		{lhVec(positionsSym("ResConfig"), positionsContractVal(lhContract(t, loUSDCSAC))), lhResConfig(1, 7_500_000, 10_000_000), ledger - 60},
	}
	for _, a := range p.accounts {
		for i := range uint32(2) {
			sums[i][0] += a.coll[i]
			sums[i][1] += a.liab[i]
		}
		out = append(out, lhEntry{lhVec(positionsSym("Positions"), positionsContractVal(a.id)), lhPositions(a.coll, a.liab), a.ledger})
	}
	for i, sac := range []string{loXLMSAC, loUSDCSAC} {
		out = append(out, lhEntry{
			lhVec(positionsSym("ResData"), positionsContractVal(lhContract(t, sac))),
			lhReserve(t, p.rate, sums[i][0], sums[i][1], p.lastTime), p.reserveLedger,
		})
	}
	return append(out, p.extra...)
}

// TestLendingHealth_EndToEnd serves /v1/lending/pools/{pool}/health over HTTP from real ClickHouse and Postgres: the
// real CEX oracle-aggregator and Reflector feed entries price XLM and leave USDC a verdict (no stored USDC round).
//
// Every main-pool account values XLM against XLM, so the oracle price P cancels and each HF is exact by hand. Rates are
// 1.0, decimals 7, and the lake's ResConfig has c_factor 0.75, l_factor 1.0 (Postgres's events say 0.95: the lake wins,
// or liq would be 95/80, healthy). Amounts are multiples of 10^7, so base = P·(amount/10^7)·factor exactly:
//
//	healthy:  collateral 100 XLM → 75P, debt 50 XLM → 50P:  75/50 = 1.5000, healthy
//	liq:      collateral 100 XLM → 75P, debt 80 XLM → 80P:  75/80 = 0.9375, liquidatable (80P > 75P)
//	auction:  collateral 100 XLM → 75P, debt 90 XLM → 90P:  75/90 = 0.8333, gate admits it but an auction is live
//	verdict:  collateral 100 USDC (oracle_feed_incomplete), debt 10 XLM: unpriced, that verdict
//	newer:    position written after the evaluation ledger: unpriced, state_newer
func TestLendingHealth_EndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c.InstallAliasRegistry(nil)
	er := loSeedLake(t, ctx)
	st := loCEXState(t, ctx, er)
	round := time.UnixMilli(int64(st.LastTimestampMs)).UTC() //nolint:gosec // a ledger-era ms timestamp
	prev := round.Add(-300 * time.Second)
	idx, _ := st.AssetIndex(oracleagg.Asset{Symbol: "XLM"})
	xlmAt := func(ts time.Time) *big.Int {
		v, ok := st.CachedPrice(idx, uint64(ts.UnixMilli())) //nolint:gosec // a ledger-era ms timestamp
		if !ok {
			t.Fatalf("XLM not in the feed's cache at %s", ts)
		}
		return v
	}
	closeAt := round.Add(100 * time.Second)
	price := new(big.Int).Quo(xlmAt(round), big.NewInt(10_000_000)) // P: the adapter's 14 → 7 decimals, truncated
	rounds := []c.OracleUpdate{
		loUpdate(t, loCEXFeed, "crypto:XLM", round, loLedger-10, xlmAt(round), 0),
		loUpdate(t, loCEXFeed, "crypto:XLM", prev, loLedger-70, xlmAt(prev), 0),
	}

	backstop := xdr.ContractId{0xBB, 0x01}
	acct := func(b byte) xdr.ContractId { return xdr.ContractId{0xD0, b} }
	healthy, liq, auction, verdict, newer := acct(1), acct(2), acct(3), acct(4), acct(5)
	const e7 = 10_000_000
	at := func(id xdr.ContractId, coll, liab map[uint32]uint64) lhAccount {
		return lhAccount{id, coll, liab, loLedger - 40}
	}
	xlm := func(n uint64) map[uint32]uint64 { return map[uint32]uint64{0: n} }
	v2 := func(id xdr.ContractId, cfg xdr.ScVal, cfgLedger uint32, accts ...lhAccount) lhPool {
		return lhPool{
			id: id, rate: "1000000000000", lastTime: closeAt.Unix(), cfgXLM: cfg, cfgXLMLedger: cfgLedger,
			accounts: accts, reserveLedger: loLedger - 50,
		}
	}
	main := v2(xdr.ContractId{0xC4, 0x11, 0x01}, lhResConfig(0, 7_500_000, 10_000_000), loLedger-60,
		at(healthy, xlm(100*e7), xlm(50*e7)), at(liq, xlm(100*e7), xlm(80*e7)), at(auction, xlm(100*e7), xlm(90*e7)),
		at(verdict, map[uint32]uint64{1: 100 * e7}, xlm(10*e7)), lhAccount{newer, xlm(10 * e7), xlm(5 * e7), loLedger + 5})
	main.extra = []lhEntry{{lhVec(positionsSym("Auction"), lendingSymMap(map[string]xdr.ScVal{"auct_type": lendingU32(0), "user": positionsContractVal(auction)},
		"auct_type", "user")), lendingSymMap(map[string]xdr.ScVal{"block": lendingU32(1)}, "block"), loLedger - 20}}
	// A ResConfig written after the evaluation ledger: its holder is state_newer, however healthy.
	cfgNewer := v2(xdr.ContractId{0xC4, 0x11, 0x02}, lhResConfig(0, 7_500_000, 10_000_000), loLedger+3, at(acct(6), xlm(100*e7), xlm(10*e7)))
	// c_factor 0.8, l_factor 0.9 (see the factors block below for the hand values).
	f0, f1, f2 := acct(7), acct(8), acct(9)
	factors := v2(xdr.ContractId{0xC4, 0x11, 0x03}, lhResConfig(0, 8_000_000, 9_000_000), loLedger-60,
		at(f0, xlm(100*e7), xlm(72*e7)), at(f1, xlm(100*e7), xlm(75*e7)), at(f2, xlm(100*e7), xlm(72*e7+1)))
	// V1: rates in 9 decimals, last written an hour before the ledger, so interest is pending and V1 has no bound.
	v1Pool := v2(xdr.ContractId{0xC4, 0x11, 0x04}, lhResConfig(0, 7_500_000, 10_000_000), loLedger-60, at(acct(10), xlm(100*e7), xlm(50*e7)))
	v1Pool.rate, v1Pool.lastTime = "1000000000", closeAt.Unix()-3600
	blntPool := v2(xdr.ContractId{0xC4, 0x11, 0x05}, lhResConfig(0, 7_500_000, 10_000_000), loLedger-60, at(acct(11), xlm(100*e7), xlm(50*e7)))

	raw := dialClickHouse(t, ctx, "stellar")
	b, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.ledger_entries_current
		(entry_type, key_xdr, account_id, asset, balance, change_type, ledger_seq, close_time, entry_xdr)`)
	if err != nil {
		t.Fatal(err)
	}
	pools := []lhPool{main, cfgNewer, factors, v1Pool, blntPool}
	for _, p := range pools {
		for _, e := range p.entries(t, lhContract(t, loCEXAdapter), backstop, loLedger) {
			key := lendingKeyB64(t, p.id, e.key)
			if e.key.Type == xdr.ScValTypeScvLedgerKeyContractInstance {
				key = lhInstanceKey(t, p.id)
			}
			if err := b.Append("contract_data", key, "", "", int64(0), "updated", e.ledger, closeAt, positionsEntryB64(t, p.id, e.key, e.val, e.ledger)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := b.Send(); err != nil {
		t.Fatal(err)
	}

	id := func(x xdr.ContractId) string { return positionsStrkey(t, x) }
	store := lhStore(t, ctx, map[string]string{
		id(main.id): blend.MainnetPoolFactory, id(cfgNewer.id): blend.MainnetPoolFactory, id(factors.id): blend.MainnetPoolFactory,
		id(v1Pool.id): blend.MainnetPoolFactoryV1, id(blntPool.id): "",
	}, round, rounds)
	srv := v1.New(v1.Options{Explorer: loLake{er, closeAt}, Lending: store, LakeWatermark: loWatermark{closeAt}})
	srv.SetLendingHealthEnabled(true)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	get := func(pool xdr.ContractId) v1.LendingPoolHealthView {
		var env struct {
			Data v1.LendingPoolHealthView `json:"data"`
		}
		lhGet(t, ts.URL+"/v1/lending/pools/"+id(pool)+"/health", &env)
		return env.Data
	}

	h := get(main.id)
	if h.Basis.Ledger != loLedger || h.Version == nil || *h.Version != "v2" || h.Accrual.PendingSecondsMax != 0 {
		t.Fatalf("basis %+v version %v accrual %+v", h.Basis, h.Version, h.Accrual)
	}
	p := func(k int64) string { return new(big.Int).Mul(price, big.NewInt(k)).String() }
	unpricedFirst, unpricedSecond := id(verdict), id(newer)
	if unpricedSecond < unpricedFirst {
		unpricedFirst, unpricedSecond = unpricedSecond, unpricedFirst
	}
	want := []struct{ account, class, hf, reason, coll, liab string }{
		{id(auction), "in_auction", "0.8333", "", p(75), p(90)},
		{id(liq), "liquidatable", "0.9375", "", p(75), p(80)},
		{id(healthy), "healthy", "1.5000", "", p(75), p(50)},
	}
	if len(h.Accounts) != 5 {
		t.Fatalf("accounts = %+v", h.Accounts)
	}
	for i, w := range want {
		a := h.Accounts[i]
		if a.Account != w.account || a.Class != w.class || a.HF == nil || *a.HF != w.hf || a.Reason != nil ||
			*a.CollateralBase != w.coll || *a.LiabilitiesBase != w.liab {
			t.Errorf("row %d = %+v, want %+v", i, a, w)
		}
	}
	reasons := map[string]string{id(verdict): "oracle_feed_incomplete", id(newer): "state_newer"}
	for i, acc := range []string{unpricedFirst, unpricedSecond} {
		a := h.Accounts[3+i]
		if a.Account != acc || a.Class != "unpriced" || a.HF != nil || a.Reason == nil || *a.Reason != reasons[acc] {
			t.Errorf("row %d = %+v, want unpriced %s", 3+i, a, reasons[acc])
		}
	}
	if !h.Accounts[0].InAuction {
		t.Error("the auction account is not flagged in_auction")
	}
	s := h.Summary
	if s.AccountsTotal != 5 || s.AccountsWithDebt != 4 || s.Healthy != 1 || s.Liquidatable != 1 || s.InAuction != 1 ||
		s.BadDebt != 0 || s.Indeterminate != 0 || s.Unpriced["oracle_feed_incomplete"] != 1 || s.Unpriced["state_newer"] != 1 {
		t.Fatalf("summary = %+v", s)
	}
	// Priced accounts only: collateral 3·75P, liabilities 50P + 80P + 90P.
	if s.CollateralBase != p(225) || s.LiabilitiesBase != p(220) {
		t.Fatalf("bases %s / %s, want %s / %s", s.CollateralBase, s.LiabilitiesBase, p(225), p(220))
	}
	wantExcl := []v1.LendingHealthExclusion{{Reason: "oracle_feed_incomplete", Accounts: 1}, {Reason: "state_newer", Accounts: 1}}
	if !s.LowerBound || len(s.Excluded) != 2 || s.Excluded[0] != wantExcl[0] || s.Excluded[1] != wantExcl[1] {
		t.Fatalf("lower_bound %v excluded %+v", s.LowerBound, s.Excluded)
	}
	// [0,1) holds liq and auction (80P + 90P); [1.5,2) holds healthy.
	if h.Bands[0].Accounts != 2 || h.Bands[0].LiabilitiesBase != p(170) || h.Bands[5].Accounts != 1 || h.Bands[5].LiabilitiesBase != p(50) {
		t.Fatalf("bands = %+v", h.Bands)
	}

	onlyRow := func(name string, h v1.LendingPoolHealthView, class, reason string) {
		t.Helper()
		if len(h.Accounts) != 1 || h.Accounts[0].Class != class || h.Accounts[0].Reason == nil || *h.Accounts[0].Reason != reason {
			t.Fatalf("%s: accounts = %+v", name, h.Accounts)
		}
	}
	onlyRow("ResConfig newer", get(cfgNewer.id), "unpriced", "state_newer")
	onlyRow("BLNT", get(blntPool.id), "unpriced", "health_formula_unverified")
	onlyRow("V1", get(v1Pool.id), "indeterminate", "accrual_unbounded")

	// Factors, valued here from first principles: collateral floor(P·floor(X·0.8)/1e7), debt ceil(P·ceil(Y·1e7/9e6)/1e7).
	//	f0: X 100 XLM, Y 72 XLM: debt e = 80e7 exactly, so L = 80P = C: healthy (V2 liquidates only when L > C).
	//	f1: Y 75 XLM: L ≈ 83.33P > 80P: liquidatable; an l_factor that multiplied (67.5P) would call it healthy.
	//	f2: Y 72 XLM + 1 stroop: e = ceil(800000001.1) = 800000002, L = 80P + ceil(2P/1e7) > C: liquidatable;
	//	    flooring both steps gives 80P + floor(P/1e7) = 80P = C (P < 1e7 for XLM), healthy.
	ceil := func(n, d *big.Int) *big.Int {
		q, r := new(big.Int).QuoRem(n, d, new(big.Int))
		if r.Sign() > 0 {
			q.Add(q, big.NewInt(1))
		}
		return q
	}
	mul := func(a, b *big.Int) *big.Int { return new(big.Int).Mul(a, b) }
	e7b, l9, x := big.NewInt(e7), big.NewInt(9_000_000), big.NewInt(100*e7)
	cBase := new(big.Int).Quo(mul(price, new(big.Int).Quo(mul(x, big.NewInt(8_000_000)), e7b)), e7b)
	if price.Cmp(e7b) >= 0 || cBase.Cmp(mul(price, big.NewInt(80))) != 0 {
		t.Fatalf("fixture assumes P < 1e7 and C = 80P; P = %s C = %s", price, cBase)
	}
	fh := get(factors.id)
	rows := map[string]v1.LendingHealthAccount{}
	for _, a := range fh.Accounts {
		rows[a.Account] = a
	}
	for acc, y := range map[xdr.ContractId]int64{f0: 72 * e7, f1: 75 * e7, f2: 72*e7 + 1} {
		lBase := ceil(mul(price, ceil(mul(big.NewInt(y), e7b), l9)), e7b)
		class := "healthy"
		if lBase.Cmp(cBase) > 0 {
			class = "liquidatable"
		}
		a := rows[id(acc)]
		if a.Class != class || *a.CollateralBase != cBase.String() || *a.LiabilitiesBase != lBase.String() {
			t.Errorf("factors %s: %+v, want %s %s/%s", id(acc), a, class, cBase, lBase)
		}
	}
	if rows[id(f0)].Class != "healthy" || rows[id(f1)].Class != "liquidatable" || rows[id(f2)].Class != "liquidatable" {
		t.Fatalf("factor fixtures do not split as designed: %+v", rows)
	}
}

func lhGet(t *testing.T, url string, into any) {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec,noctx // test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", url, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, into); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
}

// lhInstanceKey is the pool's instance LedgerKey, which is persistent with the instance ScVal as its key.
func lhInstanceKey(t *testing.T, pool xdr.ContractId) string {
	t.Helper()
	return lendingKeyB64(t, pool, xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance})
}

// lhStore is Postgres with each pool registered under its factory ("" is a BLNT pool, known by its events), its two
// reserves seen in position events, and both reserves' applied configs as events (queue_set_reserve then set_reserve)
// with c_factor 0.95, which the lake's ResConfig must override; plus the feed's XLM round.
func lhStore(t *testing.T, ctx context.Context, pools map[string]string, round time.Time, rounds []c.OracleUpdate) *timescale.Store {
	t.Helper()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, db := openOrientationStore(t, ctx, dsn)
	at := round.Add(-24 * time.Hour)
	n := 0
	for pool, factory := range pools {
		n++
		for i, asset := range []string{loXLMSAC, loUSDCSAC} {
			tx := pad64("h", 10*n+i)
			if factory == "" {
				if err := store.InsertBLNTEvent(ctx, timescale.BLNTEvent{
					ContractID: pool, Ledger: loLedger - 500 + uint32(i), LedgerCloseTime: at, TxHash: tx, //nolint:gosec // i < 2
					ContractRole: string(blnt.RolePool), EventKind: "supply_collateral", Pool: pool, UserAddress: loCEXAdapter,
					Asset: asset, Amount: "1", Amount2: "1",
				}); err != nil {
					t.Fatal(err)
				}
				continue
			}
			ev := blend.PositionEvent{
				Pool: pool, Kind: "supply_collateral", Asset: asset, User: loCEXAdapter,
				TokenAmount: big.NewInt(1), BOrDAmount: big.NewInt(1), Ledger: loLedger - 500 + uint32(i), TxHash: tx, Timestamp: at,
			} //nolint:gosec // i < 2
			if err := store.InsertBlendPositionEvent(ctx, domain.BlendPositionEvent(ev)); err != nil {
				t.Fatal(err)
			}
			cfg := map[string]any{
				"index": uint64(i), "decimals": uint64(7), "c_factor": uint64(9_500_000), "l_factor": uint64(10_000_000),
				"util": uint64(5_000_000), "max_util": uint64(9_500_000), "r_base": uint64(100_000), "r_one": uint64(500_000),
				"r_two": uint64(5_000_000), "r_three": uint64(15_000_000), "reactivity": uint64(100),
			}
			for j, ev := range []blend.AdminEvent{
				{ContractID: pool, Kind: blend.EventQueueSetReserve, Asset: asset, ReserveConfig: cfg},
				{ContractID: pool, Kind: blend.EventSetReserve, Asset: asset, ReserveIndex: uint32(i)}, //nolint:gosec // i < 2
			} {
				k := 10*n + 2*i + j
				ev.Ledger, ev.TxHash, ev.Timestamp = loLedger-400+uint32(k), pad64("a", k), at.Add(time.Duration(k)*time.Minute) //nolint:gosec // small
				if err := store.InsertBlendAdminEvent(ctx, domain.BlendAdminEvent(ev)); err != nil {
					t.Fatal(err)
				}
			}
		}
		if factory == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO protocol_contracts (source, contract_id, factory_id, first_ledger, observed_at)
			VALUES ($1, $2, $3, 1, now())`, blend.SourceName, pool, factory); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range rounds {
		if err := store.InsertOracleUpdate(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	return store
}
