package v1

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend/oracleagg"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	oracleCEXAdapter = "CCVTVW2CVA7JLH4ROQGP3CU4T3EXVCK66AZGSM4MUQPXAI4QHCZPOATS"
	oracleDEXAdapter = "CDN7JD6SP32RZHB3DHK4S4AWCCQF6GYIUO455UCR4RGRZILZDRD24N36"
	oracleV1Adapter  = "CATKK5ZNJCKQQWTUWIUFZMY6V6MOQUGSTFSXMNQZHVJHYF7GVV36FB3Y"
	oracleCEXFeed    = "CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN"
	oracleDEXFeed    = "CALI2BYU2JE6WVRUFYTS6MSBNEHGJ35P4AVCZYF3B6QOE3QKOB2PLE6M"
	oracleFXFeed     = "CBKGPWGKSKZF52CFHMTRR23TBWTPMRDIYZ4O2P5VS65BMHYH4DXMCJZC"
	oracleXLMSAC     = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	oracleUSDCSAC    = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	oracleEURCSAC    = "CDTKPWPLOURQA2SGTKTUQOWRCBZEORB4BWBOMJ3D3ZTQQSGE5F6JBQLV"
	oracleLedger     = uint32(66_000_000)
	// cexXLM is XLM's index in the CEX feed's asset list.
	cexXLM = 13
)

// oracleRound is a round timestamp aligned to the 300 s resolution; the watermark ledger closes 100 s after it.
var (
	oracleRound   = time.Unix(1_699_999_800, 0).UTC()
	oracleCloseAt = oracleRound.Add(100 * time.Second)
)

type fakeOracleLake struct {
	ExplorerReader
	instance    map[string]clickhouse.ContractInstance
	instanceErr map[string]error
	// persistent maps a key's base64 to its value; removed maps it to a removal ledger; fail fails it once.
	persistent map[string]scval.ScVal
	removed    map[string]uint32
	fail       map[string]bool
	reads      map[string]int
}

func (f *fakeOracleLake) ContractInstance(_ context.Context, c string) (clickhouse.ContractInstance, bool, error) {
	if err := f.instanceErr[c]; err != nil {
		return clickhouse.ContractInstance{}, false, err
	}
	i, ok := f.instance[c]
	return i, ok, nil
}

func (f *fakeOracleLake) ContractPersistentEntry(_ context.Context, _ string, k scval.ScVal) (scval.ScVal, uint32, bool, error) {
	key, _ := xdr.MarshalBase64(k)
	if f.reads == nil {
		f.reads = map[string]int{}
	}
	f.reads[key]++
	if f.fail[key] {
		delete(f.fail, key)
		return scval.ScVal{}, 0, false, errors.New("clickhouse: contract data lookup: connection reset")
	}
	if l, ok := f.removed[key]; ok {
		return scval.ScVal{}, l, false, nil
	}
	v, ok := f.persistent[key]
	return v, oracleLedger - 2000, ok, nil
}

// CloseTimeForLedger closes every ledger 5 s after the previous one, anchored at oracleLedger.
func (f *fakeOracleLake) CloseTimeForLedger(_ context.Context, seq uint32) (time.Time, bool, error) {
	return oracleCloseAt.Add(time.Duration(int64(seq)-int64(oracleLedger)) * 5 * time.Second), true, nil
}

type fakeOracleRow struct {
	contract, source, asset, quote string
	ts                             time.Time
	ledger                         uint32
	price                          int64
}

type fakeOracleFeeds struct {
	LendingReader
	rows []fakeOracleRow
	err  error
}

// OracleFeedRounds applies the SQL's filters: contract, ts windows, ledger, and the asset keys for prices.
func (f *fakeOracleFeeds) OracleFeedRounds(_ context.Context, contract string, keys []string, lastFrom, from, to time.Time, maxLedger uint32) (timescale.OracleFeedWindow, error) {
	if f.err != nil {
		return timescale.OracleFeedWindow{}, f.err
	}
	w := timescale.OracleFeedWindow{Prices: map[int64]canonical.Amount{}}
	for _, r := range f.rows {
		if r.contract != contract || r.ts.After(to) || r.ledger > maxLedger {
			continue
		}
		if !r.ts.Before(lastFrom) && r.ts.After(w.LastRound) {
			w.LastRound, w.Source = r.ts, r.source
			w.Quote, _ = canonical.ParseAsset(r.quote)
		}
		if !r.ts.Before(from) && slices.Contains(keys, r.asset) {
			w.Prices[r.ts.UnixMilli()] = canonical.NewAmount(big.NewInt(r.price))
		}
	}
	return w, nil
}

// fixtureEntry reads the instance entry of contract from a lending_oracle fixture whose entry column is col.
func fixtureInstance(t *testing.T, file string, col int, contract string) xdr.ScContractInstance {
	t.Helper()
	f, err := os.Open("../../../test/fixtures/lending_oracle/" + file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		cols := strings.Split(sc.Text(), "\t")
		if len(cols) <= col || len(cols[col]) < 40 {
			continue
		}
		var le xdr.LedgerEntry
		if err := xdr.SafeUnmarshalBase64(cols[col], &le); err != nil {
			t.Fatal(err)
		}
		cd := le.Data.MustContractData()
		id := cd.Contract.MustContractId()
		if strkey.MustEncode(strkey.VersionByteContract, id[:]) == contract {
			return cd.Val.MustInstance()
		}
	}
	t.Fatalf("no fixture instance for %s", contract)
	return xdr.ScContractInstance{}
}

// v1Entries is every persistent entry of oracleV1Adapter in v1-entries.tsv, keyed by the key ScVal's base64.
func v1Entries(t *testing.T) map[string]scval.ScVal {
	t.Helper()
	raw, err := os.ReadFile("../../../test/fixtures/lending_oracle/v1-entries.tsv")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]scval.ScVal{}
	for _, line := range strings.Split(string(raw), "\n") {
		cols := strings.Split(line, "\t")
		if len(cols) < 4 {
			continue
		}
		var le xdr.LedgerEntry
		if err := xdr.SafeUnmarshalBase64(cols[3], &le); err != nil {
			t.Fatal(err)
		}
		cd := le.Data.MustContractData()
		id := cd.Contract.MustContractId()
		if strkey.MustEncode(strkey.VersionByteContract, id[:]) != oracleV1Adapter || cd.Key.Type == xdr.ScValTypeScvLedgerKeyContractInstance {
			continue
		}
		k, _ := xdr.MarshalBase64(cd.Key)
		out[k] = cd.Val
	}
	if len(out) == 0 {
		t.Fatal("no v1 persistent entries")
	}
	return out
}

// v1Key is the base64 of the v1 adapter's persistent key variant(asset) for the XLM SAC.
func v1Key(t *testing.T, variant string) string {
	t.Helper()
	ak, err := oracleagg.AssetScVal(oracleagg.Stellar(oracleXLMSAC))
	if err != nil {
		t.Fatal(err)
	}
	k, _ := xdr.MarshalBase64(scval.NewVec(scval.NewSymbol(variant), ak))
	return k
}

// feedPeriods is a feed's chain-side history: asset index → periods before the last round it was priced in.
type feedPeriods map[uint32][]uint32

// oracleFeedStorage is the real CEX feed instance with last_timestamp, resolution and history rewritten (and the
// cache dropped) to describe a feed whose last round is last.
func oracleFeedStorage(t *testing.T, last time.Time, resMs uint32, priced feedPeriods) []scval.ScMapEntry {
	t.Helper()
	inst := fixtureInstance(t, "reflector-instances.tsv", 3, oracleCEXFeed)
	history := make([]byte, 32*16)
	for idx, periods := range priced {
		for _, p := range periods {
			history[idx*32+(31-p/8)] |= 1 << (p % 8)
		}
	}
	var out []scval.ScMapEntry
	for _, e := range *inst.Storage {
		switch name, _ := scval.AsString(e.Key); name {
		case "cache":
			continue
		case "last_timestamp":
			v := xdr.Uint64(last.UnixMilli())
			e.Val = xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &v}
		case "resolution":
			e.Val = scval.NewU32(resMs)
		case "history":
			b := xdr.ScBytes(history)
			e.Val = xdr.ScVal{Type: xdr.ScValTypeScvBytes, Bytes: &b}
		}
		out = append(out, e)
	}
	return out
}

func newOracleServer(t *testing.T, adapterHash string, rows []fakeOracleRow, feed []scval.ScMapEntry) (*Server, *fakeOracleLake) {
	t.Helper()
	adapter := func(id string) clickhouse.ContractInstance {
		return clickhouse.ContractInstance{WasmHash: adapterHash, Storage: *fixtureInstance(t, "instances.tsv", 2, id).Storage, Ledger: oracleLedger - 1000}
	}
	v1 := adapter(oracleV1Adapter)
	v1.WasmHash = oracleagg.HashV1
	lake := &fakeOracleLake{
		instance: map[string]clickhouse.ContractInstance{
			oracleCEXAdapter: adapter(oracleCEXAdapter), oracleDEXAdapter: adapter(oracleDEXAdapter), oracleV1Adapter: v1,
			oracleCEXFeed: {WasmHash: oracleagg.ReflectorV71Hash, Storage: feed, Ledger: oracleLedger - 10},
		},
		persistent: v1Entries(t), removed: map[string]uint32{}, fail: map[string]bool{},
	}
	s := &Server{Options: Options{Explorer: lake, Lending: &fakeOracleFeeds{rows: rows}, LakeWatermark: &stubWatermark{ledger: oracleLedger, closedAt: oracleCloseAt}}}
	return s, lake
}

func cexRow(contract, asset string, ts time.Time, ledger uint32, price int64) fakeOracleRow {
	return fakeOracleRow{contract: contract, source: "reflector-cex", asset: asset, quote: "fiat:USD", ts: ts, ledger: ledger, price: price}
}

func priceXLM(t *testing.T, s *Server) (LendingOraclePrice, LendingOracleBasis) {
	t.Helper()
	got, basis := s.PoolOraclePrices(context.Background(), LendingOraclePool{ID: "POOL", Oracle: oracleCEXAdapter, Reserves: []string{oracleXLMSAC}})
	return got[oracleXLMSAC], basis
}

func TestPoolOraclePrices_OwnFeedOnly(t *testing.T) {
	prev := oracleRound.Add(-300 * time.Second)
	rows := []fakeOracleRow{
		cexRow(oracleCEXFeed, "crypto:XLM", oracleRound, oracleLedger-10, 12_345_678_901_234_567),
		cexRow(oracleCEXFeed, "crypto:XLM", prev, oracleLedger-70, 12_345_678_901_234_567),
		// A foreign Reflector contract publishing the same asset must not price the pool.
		cexRow(oracleFXFeed, "crypto:USDC", oracleRound, oracleLedger-10, 1e14),
		cexRow(oracleFXFeed, "crypto:USDC", prev, oracleLedger-70, 1e14),
		// A raw row on the pool's own feed marks the round but never prices.
		cexRow(oracleCEXFeed, "raw:EURC", oracleRound, oracleLedger-10, 1e14),
		cexRow(oracleCEXFeed, "raw:EURC", prev, oracleLedger-70, 1e14),
	}
	s, _ := newOracleServer(t, oracleagg.HashV2, rows, oracleFeedStorage(t, oracleRound, 300_000, feedPeriods{cexXLM: {0, 1}}))
	got, basis := s.PoolOraclePrices(context.Background(), LendingOraclePool{
		ID: "POOL", Oracle: oracleCEXAdapter, Reserves: []string{oracleXLMSAC, oracleUSDCSAC, oracleEURCSAC},
	})
	if basis.Program != oracleagg.ProgramV2 || basis.Ledger != oracleLedger || !basis.CloseTime.Equal(oracleCloseAt) {
		t.Fatalf("basis = %+v (%+v)", basis, got)
	}
	xlm := got[oracleXLMSAC]
	// floor(12_345_678_901_234_567 / 10^7) at the adapter's 7 decimals, stamped with the period.
	if xlm.Verdict != "" || xlm.Price.Price.String() != "1234567890" || xlm.Price.Decimals != 7 ||
		xlm.Price.Timestamp != uint64(oracleRound.Unix()) || xlm.Quote.String() != "fiat:USD" {
		t.Fatalf("XLM = %+v", xlm)
	}
	for _, r := range []string{oracleUSDCSAC, oracleEURCSAC} {
		if got[r].Verdict != oracleagg.VerdictPriceStale || got[r].Price.Price != nil {
			t.Fatalf("%s = %+v, want oracle_price_stale and no price", r, got[r])
		}
	}
}

func TestPoolOraclePrices_ForeignWasmUnsupported(t *testing.T) {
	foreign := "a41fc53d6753b6c04eb15b021c55052366a4c8e0e21bc72700f461264ec1350e"
	s, _ := newOracleServer(t, foreign, nil, oracleFeedStorage(t, oracleRound, 300_000, nil))
	x, basis := priceXLM(t, s)
	if basis.WasmHash != foreign || x.Verdict != oracleagg.VerdictUnsupported || !strings.Contains(x.Detail, foreign) {
		t.Fatalf("got %+v basis %+v", x, basis)
	}
}

// The DEX adapter's base is the USDC SAC: its base reserve is 10^7 at the ledger time and the quote stays the SAC.
func TestPoolOraclePrices_DEXBaseQuoteVerbatim(t *testing.T) {
	s, _ := newOracleServer(t, oracleagg.HashV2, nil, nil)
	got, _ := s.PoolOraclePrices(context.Background(), LendingOraclePool{ID: "POOL", Oracle: oracleDEXAdapter, Reserves: []string{oracleUSDCSAC}})
	u := got[oracleUSDCSAC]
	if u.Verdict != "" || u.Price.Price.String() != "10000000" || u.Price.Timestamp != uint64(oracleCloseAt.Unix()) || u.Quote.String() != oracleUSDCSAC {
		t.Fatalf("USDC = %+v", u)
	}
}

// The projector has not written the feed's last round: the chain's last_timestamp says it priced XLM, so the reserve
// gets a verdict instead of the older stored round, and the basis stays the watermark.
func TestPoolOraclePrices_RoundMissingHere(t *testing.T) {
	rows := []fakeOracleRow{
		cexRow(oracleCEXFeed, "crypto:XLM", oracleRound.Add(-300*time.Second), oracleLedger-70, 1e14),
		cexRow(oracleCEXFeed, "crypto:XLM", oracleRound.Add(-600*time.Second), oracleLedger-130, 1e14),
	}
	s, _ := newOracleServer(t, oracleagg.HashV2, rows, oracleFeedStorage(t, oracleRound, 300_000, feedPeriods{cexXLM: {0, 1, 2}}))
	x, basis := priceXLM(t, s)
	if x.Verdict != oracleagg.VerdictFeedIncomplete || basis.Ledger != oracleLedger {
		t.Fatalf("XLM = %+v basis %+v, want oracle_feed_incomplete at the watermark", x, basis)
	}
}

// The feed's live resolution comes from its instance: at 60 s, R−300 is period 5, not period 1.
func TestPoolOraclePrices_FeedResolutionFromChain(t *testing.T) {
	rows := []fakeOracleRow{
		cexRow(oracleCEXFeed, "crypto:XLM", oracleRound, oracleLedger-10, 2e14),
		cexRow(oracleCEXFeed, "crypto:XLM", oracleRound.Add(-300*time.Second), oracleLedger-70, 2e14),
	}
	s, _ := newOracleServer(t, oracleagg.HashV2, rows, oracleFeedStorage(t, oracleRound, 60_000, feedPeriods{cexXLM: {0, 5}}))
	if x, _ := priceXLM(t, s); x.Verdict != "" || x.Price.Price.String() != "20000000" {
		t.Fatalf("XLM = %+v", x)
	}
}

// An adapter or feed entry modified after the evaluation ledger is not the state there.
func TestPoolOraclePrices_StateNewerThanLedger(t *testing.T) {
	rows := []fakeOracleRow{cexRow(oracleCEXFeed, "crypto:XLM", oracleRound, oracleLedger-10, 1e14)}
	for _, who := range []string{oracleCEXFeed, oracleCEXAdapter} {
		s, lake := newOracleServer(t, oracleagg.HashV2, rows, oracleFeedStorage(t, oracleRound, 300_000, feedPeriods{cexXLM: {0}}))
		e := lake.instance[who]
		e.Ledger = oracleLedger + 1
		lake.instance[who] = e
		if x, basis := priceXLM(t, s); x.Verdict != oracleagg.VerdictStateNewer || basis.Ledger != oracleLedger {
			t.Fatalf("%s newer: XLM = %+v basis %+v", who, x, basis)
		}
	}
}

func priceV1XLM(s *Server) LendingOraclePrice {
	got, _ := s.PoolOraclePrices(context.Background(), LendingOraclePool{ID: "POOL", Oracle: oracleV1Adapter, Reserves: []string{oracleXLMSAC}})
	return got[oracleXLMSAC]
}

// A failed AssetConfig read is ours: it fails that reserve and is not cached, so the next call re-reads it.
func TestPoolOraclePrices_V1ReadFailureNotCached(t *testing.T) {
	rows := []fakeOracleRow{cexRow(oracleCEXFeed, "crypto:XLM", oracleRound, oracleLedger-10, 1e14)}
	s, lake := newOracleServer(t, oracleagg.HashV2, rows, oracleFeedStorage(t, oracleRound, 300_000, feedPeriods{cexXLM: {0}}))
	key := v1Key(t, "AssetConfig")
	lake.fail[key] = true
	if x := priceV1XLM(s); x.Verdict != oracleagg.VerdictReadFailed {
		t.Fatalf("first read: XLM = %+v, want oracle_read_failed", x)
	}
	if x := priceV1XLM(s); x.Verdict != "" || x.Price.Price.String() != "10000000" || lake.reads[key] != 2 {
		t.Fatalf("second read: XLM = %+v after %d AssetConfig reads, want a price after 2", x, lake.reads[key])
	}
}

// Blocked(XLM) removed after the watermark: at that ledger the asset may have been blocked.
func TestPoolOraclePrices_V1EntryRemovedAfterWatermark(t *testing.T) {
	s, lake := newOracleServer(t, oracleagg.HashV2, nil, oracleFeedStorage(t, oracleRound, 300_000, feedPeriods{cexXLM: {0}}))
	lake.removed[v1Key(t, "Blocked")] = oracleLedger + 1
	if x := priceV1XLM(s); x.Verdict != oracleagg.VerdictStateNewer {
		t.Fatalf("XLM = %+v, want oracle_state_newer", x)
	}
}

// An archived feed instance is chain state and conflicting stored rows are a data conflict; neither is our read failing.
func TestPoolOraclePrices_ArchivedAndConflictNamed(t *testing.T) {
	feed := oracleFeedStorage(t, oracleRound, 300_000, feedPeriods{cexXLM: {0}})
	s, lake := newOracleServer(t, oracleagg.HashV2, nil, feed)
	lake.instanceErr = map[string]error{oracleCEXFeed: fmt.Errorf("%w: feed instance", clickhouse.ErrContractEntryUnreadable)}
	if x, _ := priceXLM(t, s); x.Verdict != oracleagg.VerdictStateArchived {
		t.Fatalf("archived feed: XLM = %+v", x)
	}
	s, _ = newOracleServer(t, oracleagg.HashV2, nil, feed)
	s.Lending.(*fakeOracleFeeds).err = fmt.Errorf("%w: two prices", timescale.ErrOracleFeedAmbiguous)
	if x, _ := priceXLM(t, s); x.Verdict != oracleagg.VerdictFeedConflict {
		t.Fatalf("conflicting rows: XLM = %+v", x)
	}
}
