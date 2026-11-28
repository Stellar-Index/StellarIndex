//go:build integration

package integration_test

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend/oracleagg"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	loCEXAdapter = "CCVTVW2CVA7JLH4ROQGP3CU4T3EXVCK66AZGSM4MUQPXAI4QHCZPOATS"
	loV1Adapter  = "CATKK5ZNJCKQQWTUWIUFZMY6V6MOQUGSTFSXMNQZHVJHYF7GVV36FB3Y"
	loCEXFeed    = "CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN"
	loFXFeed     = "CBKGPWGKSKZF52CFHMTRR23TBWTPMRDIYZ4O2P5VS65BMHYH4DXMCJZC"
	loXLMSAC     = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	loUSDCSAC    = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	loEURCSAC    = "CDTKPWPLOURQA2SGTKTUQOWRCBZEORB4BWBOMJ3D3ZTQQSGE5F6JBQLV"
	loLedger     = uint32(66_000_000)
)

// loFixtureRows returns (key_xdr, entry_xdr) of every fixture entry: instances.tsv columns 0/2, v1-entries.tsv 1/3.
func loFixtureRows(t *testing.T) [][2]string {
	t.Helper()
	var out [][2]string
	for _, f := range []struct {
		name     string
		key, val int
	}{{"instances.tsv", 0, 2}, {"v1-entries.tsv", 1, 3}} {
		fh, err := os.Open(filepath.Join("..", "fixtures", "lending_oracle", f.name))
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			cols := strings.Split(sc.Text(), "\t")
			if len(cols) > f.val && len(cols[f.val]) >= 40 {
				out = append(out, [2]string{cols[f.key], cols[f.val]})
			}
		}
		_ = fh.Close()
	}
	return out
}

// loFeedInstance is a synthetic instance entry for a Reflector feed running the attested v7.1.0 build.
func loFeedInstance(t *testing.T, feed string) (key, entry string) {
	t.Helper()
	raw := strkey.MustDecode(strkey.VersionByteContract, feed)
	var id xdr.ContractId
	copy(id[:], raw)
	hb, _ := hex.DecodeString(oracleagg.ReflectorV71Hash)
	var h xdr.Hash
	copy(h[:], hb)
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id}
	instKey := xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance}
	inst := &xdr.ScContractInstance{Executable: xdr.ContractExecutable{Type: xdr.ContractExecutableTypeContractExecutableWasm, WasmHash: &h}}
	key, err := xdr.MarshalBase64(xdr.LedgerKey{Type: xdr.LedgerEntryTypeContractData, ContractData: &xdr.LedgerKeyContractData{
		Contract: addr, Key: instKey, Durability: xdr.ContractDataDurabilityPersistent,
	}})
	if err != nil {
		t.Fatal(err)
	}
	entry, err = xdr.MarshalBase64(xdr.LedgerEntry{LastModifiedLedgerSeq: 65_079_084, Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Contract: addr, Key: instKey, Durability: xdr.ContractDataDurabilityPersistent,
			Val: xdr.ScVal{Type: xdr.ScValTypeScvContractInstance, Instance: inst},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return key, entry
}

func loSeedLake(t *testing.T, ctx context.Context) *chstore.ExplorerReader {
	t.Helper()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")
	b, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.ledger_entries_current
		(entry_type, key_xdr, account_id, asset, balance, change_type, ledger_seq, close_time, entry_xdr)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	rows := loFixtureRows(t)
	fk, fe := loFeedInstance(t, loCEXFeed)
	rows = append(rows, [2]string{fk, fe})
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, r := range rows {
		if err := b.Append("contract_data", r[0], "", "", int64(0), "updated", loLedger-100, at, r[1]); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := b.Send(); err != nil {
		t.Fatalf("send: %v", err)
	}
	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })
	return er
}

func loUpdate(t *testing.T, contract, asset string, ts time.Time, ledger uint32, price int64, op uint32) c.OracleUpdate {
	t.Helper()
	a, err := c.ParseAsset(asset)
	if err != nil {
		t.Fatal(err)
	}
	usd, _ := c.NewFiatAsset("USD")
	return c.OracleUpdate{
		Source: reflector.SourceCEX, ContractID: contract, Ledger: ledger, OpIndex: op,
		TxHash: fmt.Sprintf("%064x", ledger), Timestamp: ts, Asset: a, Quote: usd,
		Price: c.NewAmount(big.NewInt(price)), Decimals: 14,
	}
}

// TestLendingOracle_EndToEnd runs the pool-oracle path on real ClickHouse and Postgres: the identity gate and config
// from the adapter's own real entries by exact key, then the feed rows keyed on the Reflector CONTRACT, then the recipe.
func TestLendingOracle_EndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c.InstallAliasRegistry(nil)
	er := loSeedLake(t, ctx)

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	round := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) // aligned to 300 s
	prev := round.Add(-300 * time.Second)
	seeds := []c.OracleUpdate{
		loUpdate(t, loCEXFeed, "crypto:XLM", round, loLedger-10, 12_345_678_901_234_567, 0),
		loUpdate(t, loCEXFeed, "crypto:XLM", prev, loLedger-70, 12_345_678_901_234_567, 0),
		// Same asset, same source name, a foreign contract: must not price the pool.
		loUpdate(t, loFXFeed, "crypto:USDC", round, loLedger-10, 1e14, 1),
		loUpdate(t, loFXFeed, "crypto:USDC", prev, loLedger-70, 1e14, 1),
		// A raw row on the pool's own feed: marks the round, never prices.
		loUpdate(t, loCEXFeed, "raw:EURC", round, loLedger-10, 1e14, 2),
		loUpdate(t, loCEXFeed, "raw:EURC", prev, loLedger-70, 1e14, 2),
		// Applied after the evaluation ledger: invisible to it.
		loUpdate(t, loCEXFeed, "crypto:XLM", round.Add(300*time.Second), loLedger+5, 9e14, 0),
	}
	for _, u := range seeds {
		if err := store.InsertOracleUpdate(ctx, u); err != nil {
			t.Fatalf("InsertOracleUpdate: %v", err)
		}
	}

	hash, err := er.ContractWasmHash(ctx, loCEXAdapter)
	if err != nil || oracleagg.ProgramForHash(hash) != oracleagg.ProgramV2 {
		t.Fatalf("adapter hash %q (%v) is not the audited v2 build", hash, err)
	}
	feedHash, err := er.ContractWasmHash(ctx, loCEXFeed)
	if err != nil {
		t.Fatal(err)
	}
	storage, ok, err := er.ContractInstanceStorage(ctx, loCEXAdapter)
	if err != nil || !ok {
		t.Fatalf("instance storage: ok=%v err=%v", ok, err)
	}
	cfg, err := oracleagg.DecodeV2Instance(storage)
	if err != nil {
		t.Fatal(err)
	}

	tClose := round.Add(100 * time.Second)
	price := func(reserve string) oracleagg.Result {
		rule := cfg.Assets[oracleagg.Stellar(reserve)]
		key, err := oracleagg.FeedAssetKey(rule.FeedAsset)
		if err != nil {
			t.Fatal(err)
		}
		w, err := store.OracleFeedRounds(ctx, rule.Feed, c.AssetAliasStrings(key), tClose.Add(-2*time.Hour), tClose, loLedger)
		if err != nil {
			t.Fatal(err)
		}
		if !w.LastRound.Equal(round) || w.Source != reflector.SourceCEX {
			t.Fatalf("%s: last round %s source %q, want %s from the own feed at or before L%d", reserve, w.LastRound, w.Source, round, loLedger)
		}
		f := oracleagg.FeedFromRounds(oracleagg.FeedProgramForHash(feedHash), reflector.DefaultResolutionSeconds, w.LastRound, w.Quote, key, w.Prices)
		return oracleagg.LastPrice(cfg, oracleagg.Stellar(reserve), uint64(tClose.Unix()), f)
	}
	if x := price(loXLMSAC); x.Verdict != "" || x.Price.Price.String() != "1234567890" || x.Price.Timestamp != uint64(round.Unix()) {
		t.Fatalf("XLM = %+v, want floor(12345678901234567/10^7) at the round", x)
	}
	for _, r := range []string{loUSDCSAC, loEURCSAC} {
		if got := price(r); got.Verdict != oracleagg.VerdictPriceStale {
			t.Fatalf("%s = %+v, want oracle_price_stale (no own-feed mapped row)", r, got)
		}
	}
}

// TestLendingOracle_V1ConfigFromLake decodes a v1 adapter from its real instance and persistent entries, read one
// exact key at a time, to the audited configuration.
func TestLendingOracle_V1ConfigFromLake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	er := loSeedLake(t, ctx)
	storage, ok, err := er.ContractInstanceStorage(ctx, loV1Adapter)
	if err != nil || !ok {
		t.Fatalf("instance storage: ok=%v err=%v", ok, err)
	}
	cfg, err := oracleagg.DecodeV1(storage, func(k scval.ScVal) (scval.ScVal, bool, error) {
		return er.ContractPersistentEntry(ctx, loV1Adapter, k)
	})
	if err != nil {
		t.Fatal(err)
	}
	rule := func(sym string) oracleagg.AssetRule {
		return oracleagg.AssetRule{Feed: loCEXFeed, FeedAsset: oracleagg.Asset{Symbol: sym}, FeedDecimals: 14, ResolutionSeconds: 300}
	}
	want := oracleagg.Config{
		Program: oracleagg.ProgramV1, Base: oracleagg.Asset{Symbol: "USD"}, Decimals: 7,
		Assets: map[oracleagg.Asset]oracleagg.AssetRule{oracleagg.Stellar(loXLMSAC): rule("XLM"), oracleagg.Stellar(loUSDCSAC): rule("USDC")},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got  %+v\nwant %+v", cfg, want)
	}
}
