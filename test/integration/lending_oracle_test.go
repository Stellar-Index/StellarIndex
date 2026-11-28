//go:build integration

package integration_test

import (
	"bufio"
	"context"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
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

// loFixtureRows returns (key_xdr, entry_xdr) of every fixture entry: instances.tsv columns 0/2, v1-entries.tsv 1/3,
// reflector-instances.tsv 1/3 (the real CEX and DEX feed instances).
func loFixtureRows(t *testing.T) [][2]string {
	t.Helper()
	var out [][2]string
	for _, f := range []struct {
		name     string
		key, val int
	}{{"instances.tsv", 0, 2}, {"v1-entries.tsv", 1, 3}, {"reflector-instances.tsv", 1, 3}} {
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

func loSeedLake(t *testing.T, ctx context.Context) *chstore.ExplorerReader {
	t.Helper()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")
	b, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.ledger_entries_current
		(entry_type, key_xdr, account_id, asset, balance, change_type, ledger_seq, close_time, entry_xdr)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, r := range loFixtureRows(t) {
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

func loUpdate(t *testing.T, contract, asset string, ts time.Time, ledger uint32, price *big.Int, op uint32) c.OracleUpdate {
	t.Helper()
	a, err := c.ParseAsset(asset)
	if err != nil {
		t.Fatal(err)
	}
	usd, _ := c.NewFiatAsset("USD")
	return c.OracleUpdate{
		Source: reflector.SourceCEX, ContractID: contract, Ledger: ledger, OpIndex: op,
		TxHash: fmt.Sprintf("%064x", ledger), Timestamp: ts, Asset: a, Quote: usd,
		Price: c.NewAmount(price), Decimals: 14,
	}
}

// loLake is the real lake reader with ledger close times pinned, since the fixture lake holds no ledgers.
type loLake struct {
	*chstore.ExplorerReader
	closeAt time.Time
}

func (l loLake) CloseTimeForLedger(context.Context, uint32) (time.Time, bool, error) {
	return l.closeAt, true, nil
}

type loWatermark struct{ closeAt time.Time }

func (w loWatermark) LakeWatermark(context.Context) (uint32, time.Time, error) {
	return loLedger, w.closeAt, nil
}

// loCEXState is the real CEX feed's state, decoded from the fixture: its last round and the prices the chain cached.
func loCEXState(t *testing.T, ctx context.Context, er *chstore.ExplorerReader) oracleagg.FeedState {
	t.Helper()
	inst, ok, err := er.ContractInstance(ctx, loCEXFeed)
	if err != nil || !ok || inst.WasmHash != oracleagg.ReflectorV71Hash {
		t.Fatalf("feed instance: ok=%v err=%v hash=%s", ok, err, inst.WasmHash)
	}
	st, err := oracleagg.DecodeFeedState(inst.Storage)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestLendingOracle_EndToEnd runs PoolOraclePrices on real ClickHouse and Postgres: the identity gate, config and feed
// state from the real entries by exact key, the feed rows through the production OracleFeedRounds SQL keyed on the
// Reflector CONTRACT, cross-checked against the feed's own history mask and cache.
func TestLendingOracle_EndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c.InstallAliasRegistry(nil)
	er := loSeedLake(t, ctx)
	st := loCEXState(t, ctx, er)
	round := time.UnixMilli(int64(st.LastTimestampMs)).UTC()
	prev := round.Add(-300 * time.Second)
	cachedAt := func(sym string) func(time.Time) *big.Int {
		idx, _ := st.AssetIndex(oracleagg.Asset{Symbol: sym})
		return func(ts time.Time) *big.Int {
			p, ok := st.CachedPrice(idx, uint64(ts.UnixMilli()))
			if !ok {
				t.Fatalf("%s not in the chain cache at %s", sym, ts)
			}
			return p
		}
	}
	xlmAt, usdcAt := cachedAt("XLM"), cachedAt("USDC")

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seeds := []c.OracleUpdate{
		loUpdate(t, loCEXFeed, "crypto:XLM", round, loLedger-10, xlmAt(round), 0),
		loUpdate(t, loCEXFeed, "crypto:XLM", prev, loLedger-70, xlmAt(prev), 0),
		// Same asset, same source name, the very prices the pool's feed cached, a foreign contract: the chain
		// cross-check cannot tell these apart, so only the contract key keeps them from pricing the pool.
		loUpdate(t, loFXFeed, "crypto:USDC", round, loLedger-10, usdcAt(round), 1),
		loUpdate(t, loFXFeed, "crypto:USDC", prev, loLedger-70, usdcAt(prev), 1),
		// A raw row on the pool's own feed: marks the round, never prices.
		loUpdate(t, loCEXFeed, "raw:EURC", round, loLedger-10, big.NewInt(1e14), 2),
		// Applied after the evaluation ledger: invisible to it.
		loUpdate(t, loCEXFeed, "crypto:XLM", round.Add(300*time.Second), loLedger+5, big.NewInt(9e14), 0),
	}
	for _, u := range seeds {
		if err := store.InsertOracleUpdate(ctx, u); err != nil {
			t.Fatalf("InsertOracleUpdate: %v", err)
		}
	}

	closeAt := round.Add(100 * time.Second)
	srv := v1.New(v1.Options{Explorer: loLake{er, closeAt}, Lending: store, LakeWatermark: loWatermark{closeAt}})
	got, basis := srv.PoolOraclePrices(ctx, v1.LendingOraclePool{ID: "POOL", Oracle: loCEXAdapter, Reserves: []string{loXLMSAC, loUSDCSAC, loEURCSAC}})
	if basis.Program != oracleagg.ProgramV2 || basis.Ledger != loLedger {
		t.Fatalf("basis = %+v", basis)
	}
	want := new(big.Int).Quo(xlmAt(round), big.NewInt(10_000_000)) // 14 → 7 decimals, truncated
	if x := got[loXLMSAC]; x.Verdict != "" || x.Price.Price.Cmp(want) != 0 || x.Price.Timestamp != uint64(round.Unix()) {
		t.Fatalf("XLM = %+v, want %s at the round", x, want)
	}
	// The chain priced USDC and EURC in its last round; we hold no mapped row of either from this feed.
	for _, r := range []string{loUSDCSAC, loEURCSAC} {
		if g := got[r]; g.Verdict != oracleagg.VerdictFeedIncomplete || g.Price.Price != nil {
			t.Fatalf("%s = %+v, want oracle_feed_incomplete", r, g)
		}
	}
}

// TestLendingOracle_V1ConfigFromLake decodes a v1 adapter from its real instance and persistent entries, read one
// exact key at a time, to the audited configuration.
func TestLendingOracle_V1ConfigFromLake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	er := loSeedLake(t, ctx)
	inst, ok, err := er.ContractInstance(ctx, loV1Adapter)
	if err != nil || !ok || oracleagg.ProgramForHash(inst.WasmHash) != oracleagg.ProgramV1 {
		t.Fatalf("instance: ok=%v err=%v hash=%s", ok, err, inst.WasmHash)
	}
	cfg, err := oracleagg.DecodeV1(inst.Storage, func(k scval.ScVal) (scval.ScVal, bool, error) {
		v, modified, ok, err := er.ContractPersistentEntry(ctx, loV1Adapter, k)
		if err == nil && ok && (modified == 0 || modified > loLedger) {
			t.Errorf("entry %s last modified at L%d", scval.Display(k), modified)
		}
		return v, ok, err
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

// TestOracleFeedRounds_PrunesToWindow measures both OracleFeedRounds statements, as prepared by the production call,
// over 120 days of two feeds' 5-minute rounds with the r1 compression policy: each must scan only the chunks its ts
// window touches, in the hot tier and in the compressed one.
func TestOracleFeedRounds_PrunesToWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()
	db.SetMaxOpenConns(1)

	now := time.Now().UTC().Truncate(time.Hour)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO oracle_updates (source, contract_id, ledger, tx_hash, op_index, ts, asset, quote, price, decimals)
		SELECT 'reflector-cex', f.contract, (extract(epoch FROM g.ts)::bigint / 5)::int, md5(f.contract || a.asset || g.ts::text) || md5(g.ts::text),
		       array_position($2::text[], a.asset), g.ts, a.asset, 'fiat:USD', 1000, 14
		  FROM unnest($1::text[]) AS f(contract), unnest($2::text[]) AS a(asset),
		       generate_series($3::timestamptz, $4::timestamptz, '5 minutes') AS g(ts)`,
		"{"+loCEXFeed+","+loFXFeed+"}", "{crypto:XLM,crypto:USDC,crypto:BTC}", now.Add(-120*24*time.Hour), now); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var compressed int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT compress_chunk(c)
		FROM show_chunks('oracle_updates', older_than => now() - INTERVAL '7 days') c) s`).Scan(&compressed); err != nil || compressed < 2 {
		t.Fatalf("compressed %d chunks (%v), want >= 2", compressed, err)
	}
	if _, err := db.ExecContext(ctx, `ANALYZE oracle_updates`); err != nil {
		t.Fatal(err)
	}

	keys := []string{"crypto:XLM", "native"}
	var seen []string
	for _, at := range []time.Time{now.Add(-time.Hour), now.Add(-60 * 24 * time.Hour)} {
		to := at.Add(100 * time.Second)
		from := to.Add(-2100 * time.Second)
		w, err := store.OracleFeedRounds(ctx, loCEXFeed, keys, at, from, to, ^uint32(0)>>1)
		if err != nil {
			t.Fatal(err)
		}
		if !w.LastRound.Equal(at) || len(w.Prices) != 7 {
			t.Fatalf("at %s: last round %s, %d prices; want %s and the 7 rounds in [from, to]", at, w.LastRound, len(w.Prices), at)
		}
		if seen == nil {
			seen = []string{
				capturePreparedStatement(t, ctx, db, nil, "FROM oracle_updates", "contract_id = $1", "LIMIT 1"),
				capturePreparedStatement(t, ctx, db, nil, "FROM oracle_updates", "asset = ANY($1)", "contract_id = $2"),
			}
		}
		lit := func(ts time.Time) string { return ts.Format(time.RFC3339Nano) }
		for i, args := range [][]any{
			{loCEXFeed, lit(at), lit(to), int(^uint32(0) >> 1)},
			{"{" + strings.Join(keys, ",") + "}", loCEXFeed, lit(from), lit(to), int(^uint32(0) >> 1)},
		} {
			pc := explainPrepared(t, ctx, db, "force_custom_plan", seen[i], args)
			t.Logf("query %d at %s: %s", i, at, pc)
			// A sub-hour window touches at most two 1-day chunks; a third means the ts bound did not prune.
			if pc.chunkScans > 2 {
				t.Errorf("query %d at %s scanned %d chunks, want <= 2: %s", i, at, pc.chunkScans, pc)
			}
		}
	}
}
