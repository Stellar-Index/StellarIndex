package redstone

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Build helpers ─────────────────────────────────────────────────

func marshalB64(t *testing.T, sv xdr.ScVal) string {
	t.Helper()
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func addressScVal(t *testing.T, gStrkey string) xdr.ScVal {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteAccountID, gStrkey)
	if err != nil {
		t.Fatalf("decode strkey: %v", err)
	}
	var pub xdr.Uint256
	copy(pub[:], raw)
	aid := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pub}
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &aid}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}
}

// symMap builds an ScVal::Map with symbol keys, in the given order.
func symMap(keys []string, vals []xdr.ScVal) xdr.ScVal {
	m := make(xdr.ScMap, len(keys))
	for i, k := range keys {
		sym := xdr.ScSymbol(k)
		m[i] = xdr.ScMapEntry{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}, Val: vals[i]}
	}
	pm := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &pm}
}

func vecScVal(items []xdr.ScVal) xdr.ScVal {
	vec := xdr.ScVec(items)
	pvec := &vec
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pvec}
}

// encodeAddressArg is the base64 SCVal::Address OpArgs[0] form of a relayer G-strkey.
func encodeAddressArg(t *testing.T, gStrkey string) string {
	t.Helper()
	return marshalB64(t, addressScVal(t, gStrkey))
}

// encodeStringVecArg is the base64 SCVal::Vec<String> write_prices feed_ids arg.
func encodeStringVecArg(t *testing.T, feedIDs []string) string {
	t.Helper()
	items := make([]xdr.ScVal, len(feedIDs))
	for i, id := range feedIDs {
		s := xdr.ScString(id)
		items[i] = xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &s}
	}
	return marshalB64(t, vecScVal(items))
}

// encodePayloadArg is the base64 ScBytes args[2]; the decoder never inspects it.
func encodePayloadArg(t *testing.T) string {
	t.Helper()
	b := xdr.ScBytes{0xAA, 0xBB}
	return marshalB64(t, xdr.ScVal{Type: xdr.ScValTypeScvBytes, Bytes: &b})
}

// encodeWritePricesBody builds the WritePrices event body
// Map { "updater": Address, "updated_feeds": Vec<PriceData> }; prices are
// *big.Int to exercise the U256 path (Redstone scale is 8 decimals).
func encodeWritePricesBody(t *testing.T, updater string, prices []*big.Int, packageTs, writeTs uint64) string {
	t.Helper()
	items := make([]xdr.ScVal, len(prices))
	for i, p := range prices {
		pkgU, wrU := xdr.Uint64(packageTs), xdr.Uint64(writeTs)
		items[i] = symMap(
			[]string{"price", "package_timestamp", "write_timestamp"},
			[]xdr.ScVal{
				u256ScVal(t, p),
				{Type: xdr.ScValTypeScvU64, U64: &pkgU},
				{Type: xdr.ScValTypeScvU64, U64: &wrU},
			})
	}
	return marshalB64(t, symMap(
		[]string{"updated_feeds", "updater"},
		[]xdr.ScVal{vecScVal(items), addressScVal(t, updater)}))
}

// u256ScVal builds an ScVal::U256 from a non-negative *big.Int.
func u256ScVal(t *testing.T, n *big.Int) xdr.ScVal {
	t.Helper()
	if n.Sign() < 0 {
		t.Fatalf("u256 does not accept negative: %s", n)
	}
	buf := n.Bytes()
	if len(buf) > 32 {
		t.Fatalf("value exceeds 256 bits: %s", n)
	}
	padded := make([]byte, 32)
	copy(padded[32-len(buf):], buf)
	parts := xdr.UInt256Parts{
		HiHi: xdr.Uint64(beUint64(padded[0:8])),
		HiLo: xdr.Uint64(beUint64(padded[8:16])),
		LoHi: xdr.Uint64(beUint64(padded[16:24])),
		LoLo: xdr.Uint64(beUint64(padded[24:32])),
	}
	return xdr.ScVal{Type: xdr.ScValTypeScvU256, U256: &parts}
}

func beUint64(b []byte) uint64 {
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v
}

const (
	adapterC  = "CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG" // real mainnet adapter, docs/protocols/redstone.md
	oneBTCAt8 = 50_000_000_000_000                                         // $500,000 at 8 decimals
	oneETHAt8 = 3_500_000_000_000                                          // $35,000 at 8 decimals
)

// relayerG is strkey-encoded at init from a fixed seed: a hardcoded string
// invites checksum drift.
var relayerG = func() string {
	seed := [32]byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10,
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
		0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F, 0x20,
	}
	s, err := strkey.Encode(strkey.VersionByteAccountID, seed[:])
	if err != nil {
		panic("strkey encode of fixed seed failed: " + err.Error())
	}
	return s
}()

// batchEvent builds a relayer write_prices event for feedIDs/prices (len must match
// unless the test wants a mismatch) with the given package/write timestamps.
func batchEvent(t *testing.T, feedIDs []string, prices []int64, pkgTs, wrTs uint64) *events.Event {
	t.Helper()
	bis := make([]*big.Int, len(prices))
	for i, p := range prices {
		bis[i] = big.NewInt(p)
	}
	return &events.Event{
		Topic: []string{TopicSymbolRedstone},
		Value: encodeWritePricesBody(t, relayerG, bis, pkgTs, wrTs),
		OpArgs: []string{
			encodeAddressArg(t, relayerG),
			encodeStringVecArg(t, feedIDs),
			encodePayloadArg(t),
		},
		ContractID:     adapterC,
		Ledger:         52_000_000,
		TxHash:         "abcd",
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	}
}

// decodeBatch decodes a one-package batch and fails the test on error.
func decodeBatch(t *testing.T, feedIDs []string, prices []int64) []canonical.OracleUpdate {
	t.Helper()
	updates, err := decodeWritePrices(batchEvent(t, feedIDs, prices, 1, 2), time.Now())
	if err != nil {
		t.Fatalf("decodeWritePrices: %v", err)
	}
	return updates
}

// ─── Tests ───────────────────────────────────────────────────────

func TestClassify_MatchesRedstone(t *testing.T) {
	if !classify(&events.Event{Topic: []string{TopicSymbolRedstone}}) {
		t.Errorf("expected classify true for REDSTONE topic")
	}
	if classify(&events.Event{Topic: []string{"AAAACwAAAAhTT1JPU1dBUAAAAAA="}}) {
		t.Errorf("expected classify false for non-REDSTONE")
	}
}

func TestDecode_HappyPath_TwoKnownFeeds(t *testing.T) {
	const pkgTs = uint64(1_745_000_000_000) // ms
	ev := batchEvent(t, []string{"BTC", "ETH"}, []int64{oneBTCAt8, oneETHAt8}, pkgTs, 1_745_000_060_000)
	closedAt, _ := time.Parse(time.RFC3339, ev.LedgerClosedAt)

	updates, err := decodeWritePrices(ev, closedAt)
	if err != nil {
		t.Fatalf("decodeWritePrices: %v", err)
	}
	if len(updates) != 2 {
		t.Fatalf("expected 2 updates, got %d", len(updates))
	}
	if !updates[0].Asset.Equal(mustCrypto("BTC")) {
		t.Errorf("updates[0].Asset = %+v want BTC", updates[0].Asset)
	}
	if updates[0].Price.BigInt().Cmp(big.NewInt(oneBTCAt8)) != 0 {
		t.Errorf("updates[0].Price = %s want %d", updates[0].Price, oneBTCAt8)
	}
	if updates[0].Decimals != 8 {
		t.Errorf("decimals = %d want 8", updates[0].Decimals)
	}
	// Timestamp comes from package_timestamp, not ledger close.
	if updates[0].Timestamp.UnixMilli() != int64(pkgTs) {
		t.Errorf("timestamp not from package_timestamp: got %d want %d", updates[0].Timestamp.UnixMilli(), pkgTs)
	}
	if updates[0].Observer != relayerG {
		t.Errorf("observer = %q want %q", updates[0].Observer, relayerG)
	}
	if updates[0].OpIndex != 0 || updates[1].OpIndex != 1 {
		t.Errorf("OpIndex fanout wrong: [%d, %d]", updates[0].OpIndex, updates[1].OpIndex)
	}
	if !updates[1].Asset.Equal(mustCrypto("ETH")) {
		t.Errorf("updates[1].Asset = %+v want ETH", updates[1].Asset)
	}
}

// Two events from the SAME operation but different EventIndex must get
// disjoint OpIndex blocks, so the fan-out base cannot be OperationIndex alone.
func TestDecodeWritePrices_EventIndexPreventsSameOpCollision(t *testing.T) {
	evFirst := batchEvent(t, []string{"BTC"}, []int64{oneBTCAt8}, 1_745_000_000_000, 1_745_000_060_000)
	evFirst.OperationIndex, evFirst.EventIndex = 3, 0
	closedAt, _ := time.Parse(time.RFC3339, evFirst.LedgerClosedAt)
	updatesFirst, err := decodeWritePrices(evFirst, closedAt)
	if err != nil {
		t.Fatalf("decodeWritePrices (first): %v", err)
	}

	evSecond := *evFirst
	evSecond.EventIndex = 1
	updatesSecond, err := decodeWritePrices(&evSecond, closedAt)
	if err != nil {
		t.Fatalf("decodeWritePrices (second): %v", err)
	}
	if len(updatesFirst) != 1 || len(updatesSecond) != 1 {
		t.Fatalf("expected 1 update each, got %d and %d", len(updatesFirst), len(updatesSecond))
	}
	if updatesFirst[0].OpIndex == updatesSecond[0].OpIndex {
		t.Errorf("EventIndex 0 vs 1 in the same operation collided on OpIndex=%d — the fanout base must incorporate EventIndex",
			updatesFirst[0].OpIndex)
	}
}

func TestDecode_BatchErrors(t *testing.T) {
	// 2 prices, 1 feed id: the freshness verifier dropped one submitted feed.
	mismatch := batchEvent(t, []string{"BTC"}, []int64{oneBTCAt8, oneETHAt8}, 1, 2)
	noArgs := batchEvent(t, []string{"BTC"}, []int64{1}, 1, 2)
	noArgs.OpArgs = nil
	cases := []struct {
		name string
		ev   *events.Event
		want error
	}{
		{"feedIDCountMismatch", mismatch, ErrFeedIDCountMismatch},
		{"missingOpArgs", noArgs, ErrMissingOpArgs},
		{"nonRedstoneTopic", &events.Event{Topic: []string{"AAAADwAAAAhTT1JPU1dBUAAAAAA="}}, ErrNotRedstoneEvent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeWritePrices(tc.ev, time.Now()); !errors.Is(err, tc.want) {
				t.Errorf("expected %v, got %v", tc.want, err)
			}
		})
	}
}

// Oracle capture-totality: a feed_id outside the ADR-0028 registry is
// RECORDED verbatim as raw:<feed_id> at its own vector slot, so known feeds
// keep their OpIndex.
func TestDecode_UnknownFeedRecordedAsRaw_KnownLandsUnmoved(t *testing.T) {
	updates := decodeBatch(t, []string{"BTC", "NOTAFEED", "ETH"}, []int64{oneBTCAt8, 9_000_000, oneETHAt8})
	if len(updates) != 3 {
		t.Fatalf("expected 3 updates (BTC + raw:NOTAFEED + ETH), got %d", len(updates))
	}
	raw, _ := canonical.NewOracleRawAsset("NOTAFEED")
	if !updates[0].Asset.Equal(mustCrypto("BTC")) {
		t.Errorf("updates[0].Asset = %+v want BTC", updates[0].Asset)
	}
	if !updates[1].Asset.Equal(raw) || updates[1].Asset.IsMapped() {
		t.Errorf("updates[1].Asset = %s want %s (unmapped)", updates[1].Asset, raw)
	}
	if !updates[1].Quote.Equal(quoteUSD) {
		t.Errorf("updates[1].Quote = %s want fiat:USD (no /<QUOTE> suffix → RedStone default)", updates[1].Quote)
	}
	if updates[1].Price.BigInt().Cmp(big.NewInt(9_000_000)) != 0 {
		t.Errorf("updates[1].Price = %s want 9000000 (recorded verbatim, no Invert)", updates[1].Price)
	}
	if !updates[2].Asset.Equal(mustCrypto("ETH")) {
		t.Errorf("updates[2].Asset = %+v want ETH", updates[2].Asset)
	}
	for i, u := range updates {
		if u.OpIndex != uint32(i) {
			t.Errorf("updates[%d].OpIndex = %d, want %d (original vector slot; the raw row must not shift ETH)", i, u.OpIndex, i)
		}
	}
}

// An all-unknown batch decodes to raw rows, not ErrEmptyUpdates (every batch
// during the relayer expansion had this shape). "BENJI" alone is not a real
// feed_id; the real one is BENJI_ETHEREUM_FUNDAMENTAL.
func TestDecode_AllUnknown_RecordedAsRaw(t *testing.T) {
	updates := decodeBatch(t, []string{"BENJI", "NOTAFEED"}, []int64{1, 2})
	if len(updates) != 2 {
		t.Fatalf("expected 2 raw updates, got %d", len(updates))
	}
	for i, code := range []string{"BENJI", "NOTAFEED"} {
		raw, _ := canonical.NewOracleRawAsset(code)
		if !updates[i].Asset.Equal(raw) {
			t.Errorf("updates[%d].Asset = %s want %s", i, updates[i].Asset, raw)
		}
		if updates[i].OpIndex != uint32(i) {
			t.Errorf("updates[%d].OpIndex = %d want %d", i, updates[i].OpIndex, i)
		}
	}
}

// An unmapped feed_id with a `/<FIAT>` suffix is quoted in that fiat (EUROC/EUR
// convention) but the raw code keeps the FULL feed_id and the price is never
// inverted; a non-allow-listed suffix falls back to USD.
func TestDecode_UnknownFeedQuoteSuffix(t *testing.T) {
	updates := decodeBatch(t, []string{"XYZ/EUR", "SolvBTC.BBN_FUNDAMENTAL/NOTFIAT"}, []int64{1_234, 5_678})
	if len(updates) != 2 {
		t.Fatalf("expected 2 raw updates, got %d", len(updates))
	}
	rawEUR, _ := canonical.NewOracleRawAsset("XYZ/EUR")
	if !updates[0].Asset.Equal(rawEUR) {
		t.Errorf("updates[0].Asset = %s want %s (full feed_id, suffix NOT stripped)", updates[0].Asset, rawEUR)
	}
	if !updates[0].Quote.Equal(quoteEUR) {
		t.Errorf("updates[0].Quote = %s want fiat:EUR (from /EUR suffix)", updates[0].Quote)
	}
	if updates[0].Price.BigInt().Cmp(big.NewInt(1_234)) != 0 {
		t.Errorf("updates[0].Price = %s want 1234 verbatim (no Invert for an unmapped feed)", updates[0].Price)
	}
	rawOther, _ := canonical.NewOracleRawAsset("SolvBTC.BBN_FUNDAMENTAL/NOTFIAT")
	if !updates[1].Asset.Equal(rawOther) {
		t.Errorf("updates[1].Asset = %s want %s", updates[1].Asset, rawOther)
	}
	if !updates[1].Quote.Equal(quoteUSD) {
		t.Errorf("updates[1].Quote = %s want fiat:USD (suffix not an allow-listed fiat)", updates[1].Quote)
	}
}

// ADR-0028 registry: an RWA feed whose feed_id differs from its display name,
// the EUR-quoted EUROC feed (never hardcoded to USD), a plain RWA feed and a
// tokenized-BTC crypto feed.
func TestDecode_RWAandQuoteCurrency(t *testing.T) {
	updates := decodeBatch(t,
		[]string{"BENJI_ETHEREUM_FUNDAMENTAL", "EUROC/EUR", "GILTS", "SolvBTC"},
		[]int64{1_00000000, 1_05000000, 100_00000000, 95000_00000000})
	if len(updates) != 4 {
		t.Fatalf("expected 4 updates, got %d", len(updates))
	}
	want := []struct{ asset, quote string }{
		{"rwa:BENJI", "fiat:USD"},
		{"crypto:EUROC", "fiat:EUR"},
		{"rwa:GILTS", "fiat:USD"},
		{"crypto:SolvBTC", "fiat:USD"},
	}
	for i, w := range want {
		if updates[i].Asset.String() != w.asset {
			t.Errorf("updates[%d] asset = %s, want %s", i, updates[i].Asset, w.asset)
		}
		if updates[i].Quote.String() != w.quote {
			t.Errorf("updates[%d] quote = %s, want %s", i, updates[i].Quote, w.quote)
		}
	}
}

// The registry must hold exactly the 32 known mainnet feeds (19 from ADR-0028,
// 11 from the ledger-63624934 relayer expansion, USDT0, earnUSDC_FUNDAMENTAL);
// a drift means a feed changed without updating docs and this count.
func TestFeedRegistry_Has32Feeds(t *testing.T) {
	if len(feedRegistry) != 32 {
		t.Errorf("feedRegistry has %d feeds, want 32 — update BOTH this and the doc comment in feeds.go", len(feedRegistry))
	}
	for feedID, entry := range feedRegistry {
		if err := entry.Base.Validate(); err != nil {
			t.Errorf("feed %q base asset invalid: %v", feedID, err)
		}
		if err := entry.Quote.Validate(); err != nil {
			t.Errorf("feed %q quote asset invalid: %v", feedID, err)
		}
	}
}

// No two feed_ids may share a (Base, Quote) pair: feeds arrive in one batch, so
// a shared pair would interleave two quantities into one series (e.g.
// SolvBTC_FUNDAMENTAL NAV ratio ~1.003 vs SolvBTC_FUNDAMENTAL/USD ~65,430).
func TestFeedRegistry_UniquePairs(t *testing.T) {
	seen := make(map[string]string, len(feedRegistry))
	for feedID, entry := range feedRegistry {
		pair := entry.Base.String() + "|" + entry.Quote.String()
		if prev, dup := seen[pair]; dup {
			t.Errorf("feeds %q and %q both map to pair %s — same-batch double-write into one series", prev, feedID, pair)
		}
		seen[pair] = feedID
	}
}

// The relayer expansion (ledger 63624934) published 11 feed_ids outside the
// original registry; an all-new batch must decode, not fail whole with ErrEmptyUpdates.
// Mapping verified live (see feeds.go).
func TestDecode_ExpansionFeeds(t *testing.T) {
	feedIDs := []string{
		"EUROC", // bare: USD-quoted, unlike registered EUROC/EUR
		"USDe", "sUSDe", "savUSD_FUNDAMENTAL",
		"SolvBTC_FUNDAMENTAL/USD", "SolvBTC.BBN_FUNDAMENTAL/USD",
		"USDY_FUNDAMENTAL/USD", "USST_FUNDAMENTAL", "XAUm_FUNDAMENTAL/USD",
		"deJAAA_FUNDAMENTAL/USD", "deJTRSY_FUNDAMENTAL/USD",
	}
	// Live values from api.redstone.finance at 8 decimals.
	prices := []int64{
		1_13979753, 99984603, 1_24071513, 1_18773631, 6543063_913439, 6543063_913439,
		1_14081251, 1_00957429, 4115_66800000, 1_04038535, 1_03152715,
	}
	updates := decodeBatch(t, feedIDs, prices)
	if len(updates) != len(feedIDs) {
		t.Fatalf("expected %d updates, got %d — an expansion feed is still outside the registry", len(feedIDs), len(updates))
	}
	wantAssets := []string{
		"crypto:EUROC", // quote fiat:USD, NOT fiat:EUR (that is the EUROC/EUR feed)
		"crypto:USDe", "crypto:sUSDe", "crypto:savUSD_FUNDAMENTAL",
		"crypto:SolvBTC_FUNDAMENTAL_USD", // `/` normalized to `_`
		"crypto:SolvBTC.BBN_FUNDAMENTAL_USD",
		"rwa:USDY", "rwa:USST", "rwa:XAUm", "rwa:deJAAA", "rwa:deJTRSY",
	}
	for i, want := range wantAssets {
		if got := updates[i].Asset.String(); got != want {
			t.Errorf("feed %q → asset %s, want %s", feedIDs[i], got, want)
		}
		if got := updates[i].Quote.String(); got != "fiat:USD" {
			t.Errorf("feed %q → quote %s, want fiat:USD", feedIDs[i], got)
		}
		// None is Invert: a silent inversion would be a 1.3x to 65,000x error.
		if updates[i].Price.BigInt().Cmp(big.NewInt(prices[i])) != 0 {
			t.Errorf("feed %q price = %s, want %d unchanged", feedIDs[i], updates[i].Price, prices[i])
		}
	}
	ratio := feedRegistry["SolvBTC_FUNDAMENTAL"]
	usd := feedRegistry["SolvBTC_FUNDAMENTAL/USD"]
	if ratio.Base.Equal(usd.Base) && ratio.Quote.Equal(usd.Quote) {
		t.Error("SolvBTC_FUNDAMENTAL and SolvBTC_FUNDAMENTAL/USD share a (base, quote) pair — NAV-ratio and NAV-in-USD would interleave in one series")
	}
}

// The adapter emits an ScVal::Bytes wrapping the XDR-encoded WritePrices
// struct. Helper tests that build the Map directly once masked a bug where
// the decoder asserted Map on the outer Bytes and rejected every real event.
// Fixture: mainnet ledger 62265977 (tx 349bd590…c7a8b), one XLM update.
func TestDecode_RealMainnetEvent_BytesWrappedBody(t *testing.T) {
	body := "AAAADQAAAPgAAAARAAAAAQAAAAIAAAAPAAAADXVwZGF0ZWRfZmVlZHMAAAAAAAAQAAAAAQAAAAEAAAARAAAAAQAAAAMAAAAPAAAAEXBhY2thZ2VfdGltZXN0YW1wAAAAAAAABQAAAZ2/ru8wAAAADwAAAAVwcmljZQAAAAAAAAsAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABA0AaYAAAAA8AAAAPd3JpdGVfdGltZXN0YW1wAAAAAAUAAAGdv68KiAAAAA8AAAAHdXBkYXRlcgAAAAASAAAAAAAAAAAk1wP6EQQ6Z6YlFesVDZAkQ3o7tjIdDJoRh0/hHzC1Bw=="

	// args[0] is the REAL relayer address from the tx: the decoder enforces
	// body↔args updater agreement, so a synthetic one would (correctly) refuse.
	ev := &events.Event{
		Topic: []string{TopicSymbolRedstone},
		Value: body,
		OpArgs: []string{
			"AAAAEgAAAAAAAAAAJNcD+hEEOmemJRXrFQ2QJEN6O7YyHQyaEYdP4R8wtQc=",
			encodeStringVecArg(t, []string{"XLM"}),
			encodePayloadArg(t),
		},
		ContractID:     adapterC,
		Ledger:         62_265_977,
		TxHash:         "349bd590c679a9d69ac0ff3eb49a673f95cf9d77016fc3d019eb654c772c7a8b",
		LedgerClosedAt: "2026-04-24T13:30:13Z",
	}
	closedAt, _ := time.Parse(time.RFC3339, ev.LedgerClosedAt)

	updates, err := decodeWritePrices(ev, closedAt)
	if err != nil {
		t.Fatalf("decodeWritePrices: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	if !updates[0].Asset.Equal(mustCrypto("XLM")) {
		t.Errorf("Asset = %+v want XLM", updates[0].Asset)
	}
	if updates[0].Price.BigInt().Cmp(big.NewInt(4_349_500_000)) != 0 {
		t.Errorf("Price = %s want 4349500000", updates[0].Price)
	}
}

// mxneUSDMXNAt8 is MXNe in market-FX orientation: ~17.3911 pesos per USD at 8 decimals.
const mxneUSDMXNAt8 = int64(1_739_110_000)

// RedStone publishes MXNe as USDMXN (~17.39); the registry marks it Invert so
// the decoder must reciprocate to MXNe-in-USD (~0.0575). Un-inverted it would
// imply 1 MXNe = $17.39, a ~302x error.
func TestDecode_MXNe_InvertedToTokenInUSD(t *testing.T) {
	updates := decodeBatch(t, []string{"MXNe"}, []int64{mxneUSDMXNAt8})
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	if !updates[0].Asset.Equal(mustCrypto("MXNe")) {
		t.Errorf("asset = %s want crypto:MXNe", updates[0].Asset)
	}
	if updates[0].Quote.String() != "fiat:USD" {
		t.Errorf("quote = %s want fiat:USD", updates[0].Quote)
	}
	scaled, _ := new(big.Rat).SetFrac(updates[0].Price.BigInt(), big.NewInt(100_000_000)).Float64()
	if scaled < 0.055 || scaled > 0.060 {
		t.Errorf("MXNe-in-USD = %v, want ~0.0575 (raw USDMXN ~17.39 means the inversion did not run)", scaled)
	}
	if updates[0].Price.BigInt().Cmp(big.NewInt(mxneUSDMXNAt8)) == 0 {
		t.Error("MXNe price stored un-inverted — the Invert path did not fire")
	}
}

// Inversion is scoped to Invert feeds: CETES (dollars-per-unit, ~$0.067) must
// pass through unchanged and record no separate published_price (the price
// already IS the publisher's integer).
func TestDecode_NonInvertedCurrencyFeed_Unchanged(t *testing.T) {
	const cetesUSDAt8 = int64(6_734_600) // 0.067346 × 1e8
	updates := decodeBatch(t, []string{"CETES"}, []int64{cetesUSDAt8})
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	if updates[0].Price.BigInt().Cmp(big.NewInt(cetesUSDAt8)) != 0 {
		t.Errorf("CETES price = %s, want %d unchanged (non-Invert feed must not be reciprocated)", updates[0].Price, cetesUSDAt8)
	}
	if updates[0].Quote.String() != "fiat:USD" {
		t.Errorf("CETES quote = %s want fiat:USD", updates[0].Quote)
	}
	if got, ok := publishedPriceOf(t, updates[0]); ok {
		t.Errorf("non-Invert feed recorded published_price %q; want absent", got)
	}
}

// The exact big.Int reciprocal used by the Invert path (ADR-0003: no float or int64 truncation).
func TestReciprocalAtScale(t *testing.T) {
	// 1/2.0 at 8 decimals: raw 2e8 → 0.5e8.
	if got := reciprocalAtScale(canonical.NewAmount(big.NewInt(200_000_000)), 8); got.BigInt().Cmp(big.NewInt(50_000_000)) != 0 {
		t.Errorf("1/2.0 @1e8 = %s, want 50000000", got)
	}
	// Round half-up: 1/0.3 at scale 1e1 → 100/3 = 33.33 → 33.
	if got := reciprocalAtScale(canonical.NewAmount(big.NewInt(3)), 1); got.BigInt().Cmp(big.NewInt(33)) != 0 {
		t.Errorf("100/3 = %s, want 33 (round half-up)", got)
	}
	// Involution on a clean value: invert(invert(4.0)) == 4.0.
	inv := reciprocalAtScale(canonical.NewAmount(big.NewInt(400_000_000)), 8)
	if back := reciprocalAtScale(inv, 8); back.BigInt().Cmp(big.NewInt(400_000_000)) != 0 {
		t.Errorf("invert(invert(4.0)) = %s, want 400000000", back)
	}
}

// Real lake bytes (ledger 63,699,567): the adapter emits `{updated_feeds: [],
// updater}` when its freshness verifier drops every feed (~1.5% of all events).
// It must decode to ZERO updates with NO error, with no OpArgs: the no-op
// classification must not depend on args.
func TestDecode_EmptyOnWireBatch_IsRecognizedNoOp(t *testing.T) {
	const realBody = "AAAADQAAAGwAAAARAAAAAQAAAAIAAAAPAAAADXVwZGF0ZWRfZmVlZHMAAAAAAAAQAAAAAQAAAAAAAAAPAAAAB3VwZGF0ZXIAAAAAEgAAAAAAAAAAI55i1HMFV5Z4R37dzgSnns7qJjbxzw6uCHkzbmb6XRI="
	ev := &events.Event{
		Topic:          []string{TopicSymbolRedstone},
		Value:          realBody,
		ContractID:     adapterC,
		Ledger:         63_699_567,
		TxHash:         "efgh",
		LedgerClosedAt: "2026-07-29T09:30:00Z",
	}
	closedAt, _ := time.Parse(time.RFC3339, ev.LedgerClosedAt)

	updates, err := decodeWritePrices(ev, closedAt)
	if err != nil {
		t.Fatalf("empty on-wire batch must be a recognized no-op, got error: %v", err)
	}
	if len(updates) != 0 {
		t.Fatalf("expected 0 updates from an empty batch, got %d", len(updates))
	}
}

// USDT0 maps to its own `crypto:USDT0`, never `crypto:USDT`: different tokens
// with different peg risk, and collapsing them in the decoder is the eager
// stablecoin normalisation the aggregator, not the decoder, may decide. Absent,
// it would be recorded as raw:USDT0 and ticket the unknown-symbols alert.
func TestFeedRegistry_USDT0MapsToItsOwnAsset(t *testing.T) {
	entry, ok := feedRegistry["USDT0"]
	if !ok {
		t.Fatal("USDT0 is absent from feedRegistry — it will be recorded as raw:USDT0")
	}
	if got := entry.Base.String(); got != "crypto:USDT0" {
		t.Errorf("USDT0 base = %q, want crypto:USDT0", got)
	}
	if got := entry.Quote.String(); got != "fiat:USD" {
		t.Errorf("USDT0 quote = %q, want fiat:USD", got)
	}
	if usdt, ok := feedRegistry["USDT"]; ok && usdt.Base.Equal(entry.Base) {
		t.Error("USDT0 and USDT resolve to the SAME canonical asset — they are different tokens")
	}
	if entry.Invert {
		t.Error("USDT0 is published in USD directly; Invert must be false")
	}
}

// publishedPriceOf reads published_price off the row's JSON form, so the test
// fails at runtime (not compile time) on a build that never recorded it.
func publishedPriceOf(t *testing.T, u canonical.OracleUpdate) (string, bool) {
	t.Helper()
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	raw, ok := m["published_price"]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("published_price %s is not a decimal string: %v", raw, err)
	}
	return s, true
}

// Two distinct on-chain integers collapse to the same 8-dp reciprocal, so the
// row must carry the publisher's integer verbatim (ADR-0003) or it is lost.
func TestDecode_MXNe_PublishedIntegerRecoverable(t *testing.T) {
	const wantPrice = "5747126" // round(10^16 / r) for both r below
	for _, r := range []int64{1_740_000_000, 1_740_000_001} {
		u := decodeBatch(t, []string{"MXNe"}, []int64{r})[0]
		if got := u.Price.String(); got != wantPrice {
			t.Errorf("r=%d: price = %s, want %s (oriented reciprocal unchanged)", r, got, wantPrice)
		}
		got, ok := publishedPriceOf(t, u)
		if !ok || got != big.NewInt(r).String() {
			t.Errorf("r=%d: published_price = %q (present=%v), want the on-chain integer %d", r, got, ok, r)
		}
	}
}
