package v1

import (
	"bufio"
	"context"
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
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	oracleCEXAdapter = "CCVTVW2CVA7JLH4ROQGP3CU4T3EXVCK66AZGSM4MUQPXAI4QHCZPOATS"
	oracleDEXAdapter = "CDN7JD6SP32RZHB3DHK4S4AWCCQF6GYIUO455UCR4RGRZILZDRD24N36"
	oracleCEXFeed    = "CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN"
	oracleDEXFeed    = "CALI2BYU2JE6WVRUFYTS6MSBNEHGJ35P4AVCZYF3B6QOE3QKOB2PLE6M"
	oracleFXFeed     = "CBKGPWGKSKZF52CFHMTRR23TBWTPMRDIYZ4O2P5VS65BMHYH4DXMCJZC"
	oracleXLMSAC     = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	oracleUSDCSAC    = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	oracleEURCSAC    = "CDTKPWPLOURQA2SGTKTUQOWRCBZEORB4BWBOMJ3D3ZTQQSGE5F6JBQLV"
	oracleLedger     = uint32(66_000_000)
)

// oracleRound is a round timestamp aligned to the 300 s resolution; the watermark ledger closes 100 s after it.
var (
	oracleRound   = time.Unix(1_699_999_800, 0).UTC()
	oracleCloseAt = oracleRound.Add(100 * time.Second)
)

type fakeOracleLake struct {
	ExplorerReader
	hashes   map[string]string
	instance map[string][]scval.ScMapEntry
}

func (f *fakeOracleLake) ContractWasmHash(_ context.Context, id string) (string, error) {
	return f.hashes[id], nil
}

func (f *fakeOracleLake) ContractInstanceStorage(_ context.Context, c string) ([]scval.ScMapEntry, bool, error) {
	s, ok := f.instance[c]
	return s, ok, nil
}

func (f *fakeOracleLake) ContractPersistentEntry(context.Context, string, scval.ScVal) (scval.ScVal, bool, error) {
	return scval.ScVal{}, false, nil
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
	rows    []fakeOracleRow
	cursors map[string]uint32
}

// OracleFeedRounds applies the SQL's filters: contract, ts window, ledger, and the asset keys for prices.
func (f *fakeOracleFeeds) OracleFeedRounds(_ context.Context, contract string, keys []string, from, to time.Time, maxLedger uint32) (timescale.OracleFeedWindow, error) {
	w := timescale.OracleFeedWindow{Prices: map[int64]canonical.Amount{}}
	for _, r := range f.rows {
		if r.contract != contract || r.ts.Before(from) || r.ts.After(to) || r.ledger > maxLedger {
			continue
		}
		if r.ts.After(w.LastRound) {
			w.LastRound, w.Source = r.ts, r.source
			w.Quote, _ = canonical.ParseAsset(r.quote)
		}
		if slices.Contains(keys, r.asset) {
			w.Prices[r.ts.UnixMilli()] = canonical.NewAmount(big.NewInt(r.price))
		}
	}
	return w, nil
}

func (f *fakeOracleFeeds) GetCursor(_ context.Context, source, sub string) (timescale.Cursor, error) {
	c, ok := f.cursors[sub]
	if source != "projector" || !ok {
		return timescale.Cursor{}, timescale.ErrNotFound
	}
	return timescale.Cursor{Source: source, Sub: sub, LastLedger: c}, nil
}

func oracleInstance(t *testing.T, adapter string) []scval.ScMapEntry {
	t.Helper()
	f, err := os.Open("../../../test/fixtures/lending_oracle/instances.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		cols := strings.Split(sc.Text(), "\t")
		if len(cols) < 3 || len(cols[2]) < 40 {
			continue
		}
		var le xdr.LedgerEntry
		if err := xdr.SafeUnmarshalBase64(cols[2], &le); err != nil {
			t.Fatal(err)
		}
		cd := le.Data.MustContractData()
		id := cd.Contract.MustContractId()
		if strkey.MustEncode(strkey.VersionByteContract, id[:]) == adapter {
			return *cd.Val.MustInstance().Storage
		}
	}
	t.Fatalf("no fixture instance for %s", adapter)
	return nil
}

func newOracleServer(t *testing.T, adapterHash string, rows []fakeOracleRow, cursor uint32) *Server {
	t.Helper()
	lake := &fakeOracleLake{
		hashes: map[string]string{
			oracleCEXAdapter: adapterHash, oracleDEXAdapter: adapterHash,
			oracleCEXFeed: oracleagg.ReflectorV71Hash, oracleDEXFeed: oracleagg.ReflectorV71Hash,
		},
		instance: map[string][]scval.ScMapEntry{
			oracleCEXAdapter: oracleInstance(t, oracleCEXAdapter), oracleDEXAdapter: oracleInstance(t, oracleDEXAdapter),
		},
	}
	feeds := &fakeOracleFeeds{rows: rows, cursors: map[string]uint32{"reflector-cex": cursor, "reflector-dex": cursor}}
	return &Server{Options: Options{Explorer: lake, Lending: feeds, LakeWatermark: &stubWatermark{ledger: oracleLedger, closedAt: oracleCloseAt}}}
}

func cexRow(contract, asset string, ts time.Time, ledger uint32, price int64) fakeOracleRow {
	return fakeOracleRow{contract: contract, source: "reflector-cex", asset: asset, quote: "fiat:USD", ts: ts, ledger: ledger, price: price}
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
	s := newOracleServer(t, oracleagg.HashV2, rows, oracleLedger)
	got, basis := s.poolOraclePrices(context.Background(), lendingOraclePool{
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
	s := newOracleServer(t, foreign, []fakeOracleRow{cexRow(oracleCEXFeed, "crypto:XLM", oracleRound, oracleLedger, 1e14)}, oracleLedger)
	got, basis := s.poolOraclePrices(context.Background(), lendingOraclePool{ID: "POOL", Oracle: oracleCEXAdapter, Reserves: []string{oracleXLMSAC}})
	if basis.WasmHash != foreign || got[oracleXLMSAC].Verdict != oracleagg.VerdictUnsupported || !strings.Contains(got[oracleXLMSAC].Detail, foreign) {
		t.Fatalf("got %+v basis %+v", got, basis)
	}
}

// The DEX adapter's base is the USDC SAC: its base reserve is 10^7 at the ledger time and the quote stays the SAC.
func TestPoolOraclePrices_DEXBaseQuoteVerbatim(t *testing.T) {
	s := newOracleServer(t, oracleagg.HashV2, nil, oracleLedger)
	got, _ := s.poolOraclePrices(context.Background(), lendingOraclePool{ID: "POOL", Oracle: oracleDEXAdapter, Reserves: []string{oracleUSDCSAC}})
	u := got[oracleUSDCSAC]
	if u.Verdict != "" || u.Price.Price.String() != "10000000" || u.Price.Timestamp != uint64(oracleCloseAt.Unix()) || u.Quote.String() != oracleUSDCSAC {
		t.Fatalf("USDC = %+v", u)
	}
}

// When the feed's projector is behind the lake watermark, prices are reproduced at the cursor ledger, so a round the
// projector has not written cannot be replaced by an older one at the newer ledger's time.
func TestPoolOraclePrices_HeldBackToProjectorCursor(t *testing.T) {
	cursor := oracleLedger - 30
	rows := []fakeOracleRow{
		cexRow(oracleCEXFeed, "crypto:XLM", oracleRound, oracleLedger-10, 2e14),
		cexRow(oracleCEXFeed, "crypto:XLM", oracleRound.Add(-300*time.Second), oracleLedger-70, 1e14),
		cexRow(oracleCEXFeed, "crypto:XLM", oracleRound.Add(-600*time.Second), oracleLedger-130, 1e14),
	}
	s := newOracleServer(t, oracleagg.HashV2, rows, cursor)
	got, basis := s.poolOraclePrices(context.Background(), lendingOraclePool{ID: "POOL", Oracle: oracleCEXAdapter, Reserves: []string{oracleXLMSAC}})
	if basis.Ledger != cursor || !basis.CloseTime.Equal(oracleCloseAt.Add(-150*time.Second)) {
		t.Fatalf("basis = %+v, want ledger %d", basis, cursor)
	}
	x := got[oracleXLMSAC]
	if x.Verdict != "" || x.Price.Price.String() != "10000000" || x.Price.Timestamp != uint64(oracleRound.Add(-300*time.Second).Unix()) {
		t.Fatalf("XLM = %+v", x)
	}
}
