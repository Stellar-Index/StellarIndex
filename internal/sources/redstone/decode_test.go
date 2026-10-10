package redstone

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
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

// oracle_updates carries ts in its primary key, so a decoder change that
// shifts the ts of an already-stored event makes a re-derive INSERT a second
// row instead of conflicting. These goldens pin the exact ts per input; a
// failure here means a ts-derivation change needs its own cleanup run (see
// "Re-deriving a timestamp" in docs/architecture/ingest-pipeline.md).
func TestDecodeWritePrices_TimestampGolden(t *testing.T) {
	closedAt := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	ceil := closedAt.Add(24 * time.Hour) // literal: a change to the shared window must trip this

	for name, tc := range map[string]struct {
		pkgTsMs uint64
		want    time.Time
	}{
		"package ts wins (past)":     {1_745_000_000_000, time.UnixMilli(1_745_000_000_000)},
		"millisecond precision kept": {1_745_000_000_123, time.UnixMilli(1_745_000_000_123)},
		"zero clamps to close":       {0, closedAt},
		"pre-2001 floor clamps":      {999_999_999_999, closedAt},
		"at 2001 floor kept":         {1_000_000_000_000, time.UnixMilli(1_000_000_000_000)},
		"at close+24h kept":          {uint64(ceil.UnixMilli()), ceil},
		"close+24h+1ms clamps":       {uint64(ceil.UnixMilli()) + 1, closedAt},
		"u64 max clamps":             {^uint64(0), closedAt},
	} {
		t.Run(name, func(t *testing.T) {
			body := encodeWritePricesBody(t, relayerG,
				[]*big.Int{big.NewInt(oneBTCAt8)}, tc.pkgTsMs, 1_745_000_060_000)
			ev := &events.Event{
				Topic: []string{TopicSymbolRedstone},
				Value: body,
				OpArgs: []string{
					encodeAddressArg(t, relayerG),
					encodeStringVecArg(t, []string{"BTC"}),
					encodePayloadArg(t),
				},
				ContractID:     adapterC,
				Ledger:         52_000_000,
				TxHash:         "abcd",
				LedgerClosedAt: closedAt.Format(time.RFC3339),
			}
			got, err := decodeWritePrices(ev, closedAt)
			if err != nil {
				t.Fatalf("decodeWritePrices: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d updates, want 1", len(got))
			}
			if !got[0].Timestamp.Equal(tc.want) {
				t.Errorf("ts = %s (%d ms), want %s (%d ms)",
					got[0].Timestamp, got[0].Timestamp.UnixMilli(), tc.want, tc.want.UnixMilli())
			}
		})
	}
}

// The F1 compound (payload.go's F1 CAVEAT) driven through
// decodeWritePrices on the payload-median FALLBACK path. feed_ids are
// [BTC, ETH, USDC, XLM]; the adapter accepted BTC and XLM and dropped
// ETH and USDC, so updated_feeds carries two prices:
//
//  1. signer-filter divergence — XLM's trusted signer stored 2_000_000,
//     but the payload also carries two non-trusted XLM packages at
//     9_000_000 that the adapter filtered out and this parser does not,
//     so our XLM median is 9_000_000;
//  2. cross-feed collision — the DROPPED feed ETH's payload median is
//     2_000_000, byte-equal to XLM's stored price;
//  3. order-preserving position — ETH sits between BTC and XLM.
//
// The unique order-preserving alignment is therefore [BTC, ETH]: XLM's
// price under ETH's feed_id.
const f1PackageTs = uint64(1_745_000_000_000)

func f1FallbackEvent(t *testing.T, stateWriteFeeds ...string) *events.Event {
	t.Helper()
	feedIDs := []string{"BTC", "ETH", "USDC", "XLM"}
	body := encodeWritePricesBody(t, relayerG,
		[]*big.Int{big.NewInt(1_000_000), big.NewInt(2_000_000)}, f1PackageTs, f1PackageTs+60_000)
	payload := buildTestPayload(t, f1PackageTs, map[string][]int64{
		"BTC":  {1_000_000},
		"ETH":  {2_000_000},
		"USDC": {7_000_000},
		"XLM":  {2_000_000, 9_000_000, 9_000_000},
	})
	ev := &events.Event{
		Topic:      []string{TopicSymbolRedstone},
		Value:      body,
		ContractID: adapterC,
		Ledger:     52_000_000,
		TxHash:     "f1-fallback",
		OpArgs: []string{
			encodeAddressArg(t, relayerG),
			encodeStringVecArg(t, feedIDs),
			encodePayloadArgBytes(t, payload),
		},
	}
	for _, f := range stateWriteFeeds {
		ev.StateWriteKeys = append(ev.StateWriteKeys, adapterFeedKey(t, f))
	}
	return ev
}

// The two ways the dispatcher's enrichment degrades to the fallback
// while still plumbing keys (internal/dispatcher/state_write_keys.go):
// a restored-then-rewritten-unchanged entry of a DROPPED feed counts as
// changed (written set too large), and a per-key parse/marshal failure
// excludes an ACCEPTED feed (written set too small). Either way the
// subset's arity disagrees and the payload alignment runs — and it must
// not publish a price under a feed the state writes show was not
// accepted.
func TestDecode_F1Fallback_StateWritesRefuseDroppedFeed(t *testing.T) {
	cases := map[string][]string{
		"restored dropped feed inflates written set": {"BTC", "USDC", "XLM"},
		"excluded accepted key shrinks written set":  {"BTC"},
	}
	for name, written := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := decodeWritePrices(f1FallbackEvent(t, written...), time.Unix(1_745_000_000, 0))
			if !errors.Is(err, ErrStateWriteFeedMismatch) || !errors.Is(err, ErrFeedIDCountMismatch) {
				assets := make([]string, len(out))
				for i, u := range out {
					assets[i] = u.Asset.String()
				}
				t.Fatalf("err = %v (updates %v), want ErrFeedIDCountMismatch wrapping ErrStateWriteFeedMismatch: "+
					"ETH's stored PriceData did not change, so the fallback's [BTC ETH] alignment is a misattribution", err, assets)
			}
		})
	}
}

// The corroboration only removes attributions: when the payload medians
// are honest, the same inflated written set still lets the fallback
// attribute the accepted pair.
func TestDecode_F1Fallback_CorroboratedAlignmentStillAttributes(t *testing.T) {
	ev := f1FallbackEvent(t, "BTC", "USDC", "XLM")
	ev.OpArgs[2] = encodePayloadArgBytes(t, buildTestPayload(t, f1PackageTs, map[string][]int64{
		"BTC":  {1_000_000},
		"ETH":  {5_000_000},
		"USDC": {7_000_000},
		"XLM":  {2_000_000},
	}))
	out, err := decodeWritePrices(ev, time.Unix(1_745_000_000, 0))
	if err != nil {
		t.Fatalf("decode: %v, want the fallback to attribute BTC and XLM", err)
	}
	if len(out) != 2 || out[0].Asset.String() != "crypto:BTC" || out[1].Asset.String() != "crypto:XLM" {
		t.Fatalf("got %d updates %v, want crypto:BTC then crypto:XLM", len(out), out)
	}
	if out[1].Price.String() != "2000000" {
		t.Fatalf("XLM price = %s, want 2000000", out[1].Price)
	}
}

// With NO state-write keys (events outside every registered decoder's
// contract set, stellar-rpc fixtures, pre-plumb stored events) nothing
// can contradict the alignment, and the F1 compound still misattributes.
// This pins the residual payload.go's F1 CAVEAT documents: a change that
// closes it must fail here and update that caveat with it.
func TestDecode_F1Fallback_NoStateWriteKeys_ResidualMisattribution(t *testing.T) {
	out, err := decodeWritePrices(f1FallbackEvent(t), time.Unix(1_745_000_000, 0))
	if err != nil {
		t.Fatalf("decode: %v — the F1 residual no longer reproduces; update payload.go's F1 CAVEAT", err)
	}
	if len(out) != 2 || out[1].Asset.String() != "crypto:ETH" || out[1].Price.String() != "2000000" {
		t.Fatalf("got %v, want the documented residual: XLM's 2000000 published as crypto:ETH", out)
	}
}

// navFundamentalInFiat is the explicit, evidenced attestation list for
// the ONE case where a bare RedStone `_FUNDAMENTAL` feed may carry a
// fiat quote: the token's reserve asset genuinely IS that fiat, so the
// NAV really is a dollar (or euro) figure rather than a ratio.
//
// It exists because the `_FUNDAMENTAL` suffix alone does not say what
// the NAV is denominated in — that is a per-token fact about the
// reserve, and getting it wrong is not a rounding error but an
// order-of-magnitude lie on the wire (D8: `SolvBTC_FUNDAMENTAL`
// published 1.00295305 and was served as `quote=fiat:USD`, i.e. "a
// BTC-backed token is worth $1.00" while its own
// `SolvBTC_FUNDAMENTAL/USD` sibling said $78,313.03 — live r1
// `/v1/oracle/streams?include_unmapped=true`).
//
// Adding a feed here is therefore an assertion about the world and
// needs the evidence recorded alongside it: the feed's published value
// next to the reserve asset's own price. Anything not attested must
// name its reserve asset in [feedEntry.Quote] instead. Keep the
// wording aligned with the same feed's line in feeds.go.
var navFundamentalInFiat = map[string]string{
	"BENJI_ETHEREUM_FUNDAMENTAL": "Franklin Templeton FOBXX money-market fund token; " +
		"reserve is USD cash + T-bills and the share NAV is a dollar figure " +
		"pinned at $1.00 (lake fixture ledger 60104689: 1.00000000). No `/USD` sibling feed.",
	"iBENJI_ETHEREUM_FUNDAMENTAL": "Accruing share class of BENJI; same USD reserve, " +
		"same dollar NAV (lake fixture ledger 60104689: 1.00000000). No `/USD` sibling feed.",
	"USST_FUNDAMENTAL": "STBL treasury-backed stablecoin; reserve is US Treasuries, " +
		"NAV is the dollar value per token (live 1.0096, 2026-07-27). No `/USD` sibling feed.",
	"savUSD_FUNDAMENTAL": "Avant staked avUSD vault share; reserve is avUSD, a " +
		"USD-denominated stablecoin, so the vault exchange rate is a dollar figure " +
		"(live 1.1877, 2026-07-27 — plausible only as USD). No `/USD` sibling feed.",
	"earnUSDC_FUNDAMENTAL": "Upshift earnUSDC vault share (curated by Gami Labs + " +
		"Stake Capital); reserve is USDC, a USD stablecoin, so the vault exchange rate " +
		"is a dollar figure — the same shape as savUSD_FUNDAMENTAL above. Live " +
		"0.7176 -> 0.7200 across 2026-09-03..08 on r1, monotonically increasing on every " +
		"one of 14 observations: yield-like, not price-like, which is what rules out a " +
		"volatile non-fiat reserve. NOTE the published figure sits BELOW the vault's own " +
		"~1.01 share price; that gap is the vendor's, confirmed as their published number, " +
		"and we report an oracle as published rather than reconciling it — the decoder was " +
		"checked against BTC (78,6xx) and EUROC (1.0002) on the same oracle contract, so it " +
		"is not a scaling fault here. No `/USD` sibling feed.",
}

// TestFeedRegistry_NAVFeedsQuoteTheirReserveAsset is the class guard
// for D8. A bare `_FUNDAMENTAL` feed publishes net asset value in the
// token's RESERVE asset; registering one against `fiat:USD` when the
// reserve is a crypto asset mislabels a ratio (~1.00) as a dollar
// price and serves it on `/v1/oracle/streams` with `mapped=true`.
//
// The rule, table-driven over the WHOLE live registry so a feed added
// tomorrow is covered without touching this test:
//
//	feed_id contains `_FUNDAMENTAL`
//	  AND carries no explicit `/<QUOTE>` suffix (which would declare
//	      its own denomination on the wire)
//	  AND is registered with a fiat quote
//	⇒ it MUST appear in navFundamentalInFiat with its evidence.
//
// A new NAV feed whose reserve is not fiat therefore fails CI until
// its quote names the reserve asset; a new NAV feed whose reserve
// really is fiat fails CI until someone records why.
func TestFeedRegistry_NAVFeedsQuoteTheirReserveAsset(t *testing.T) {
	for feedID, entry := range feedRegistry {
		if !strings.Contains(feedID, "_FUNDAMENTAL") {
			continue
		}
		// `X_FUNDAMENTAL/USD` names its own quote on the wire — the
		// ADR-0028 `<BASE>/<QUOTE>` convention — so it is not a bare
		// NAV ratio and is out of scope for this rule.
		if strings.ContainsRune(feedID, '/') {
			continue
		}
		if entry.Quote.Type != canonical.AssetFiat {
			continue // quoted in a reserve asset — the corrected shape
		}
		if _, attested := navFundamentalInFiat[feedID]; !attested {
			t.Errorf("feed %q publishes a NAV and is registered against fiat quote %s "+
				"without an entry in navFundamentalInFiat: either its NAV is a RATIO in a "+
				"non-fiat reserve (quote it with that asset, as SolvBTC_FUNDAMENTAL is quoted "+
				"in crypto:BTC) or its reserve really is that fiat (record the evidence). "+
				"See D8 — mislabelling a ~1.0 ratio as a USD price serves a $1.00 claim for a "+
				"token worth ~$78,313.",
				feedID, entry.Quote.String())
		}
	}

	// Two-way lockstep: an attestation that no longer describes a
	// fiat-quoted bare NAV feed is stale and must be deleted, so the
	// list can never silently pre-authorise a future mislabel.
	for feedID := range navFundamentalInFiat {
		entry, ok := feedRegistry[feedID]
		if !ok {
			t.Errorf("navFundamentalInFiat attests feed %q, which is not in feedRegistry — stale entry", feedID)
			continue
		}
		if entry.Quote.Type != canonical.AssetFiat {
			t.Errorf("navFundamentalInFiat attests feed %q, but it is quoted in %s (not fiat) — stale entry",
				feedID, entry.Quote.String())
		}
	}
}

// TestFeedRegistry_SuffixedFeedNeverSharesQuoteWithItsBareSibling is
// the name-shape-independent half of the same guard. When RedStone
// publishes BOTH `X` and `X/<FIAT>`, the suffixed id declares the
// denomination explicitly; the bare id therefore publishes a
// DIFFERENT quantity and cannot legitimately carry the same quote.
// D8 is exactly this shape: `SolvBTC_FUNDAMENTAL` and
// `SolvBTC_FUNDAMENTAL/USD` were both registered `fiat:USD`.
func TestFeedRegistry_SuffixedFeedNeverSharesQuoteWithItsBareSibling(t *testing.T) {
	for feedID, entry := range feedRegistry {
		i := strings.LastIndexByte(feedID, '/')
		if i < 0 {
			continue
		}
		bareEntry, ok := feedRegistry[feedID[:i]]
		if !ok {
			continue // no bare sibling registered
		}
		if bareEntry.Quote.Equal(entry.Quote) {
			t.Errorf("feeds %q and %q are both quoted %s: the suffixed id already declares that "+
				"denomination, so the bare id publishes a different quantity and must be quoted "+
				"in the asset its value is denominated in (D8)",
				feedID[:i], feedID, entry.Quote.String())
		}
	}
}

// TestDecode_SolvBTCFamily_NAVRatiosQuotedInReserveAsset pins the
// corrected values end-to-end through the decoder, using the four
// SolvBTC-family feeds exactly as observed live on r1 via
// `/v1/oracle/streams?include_unmapped=true`:
//
//	crypto:SolvBTC.BBN_FUNDAMENTAL       1.00000000
//	crypto:SolvBTC.BBN_FUNDAMENTAL_USD  78313.02974310
//	crypto:SolvBTC_FUNDAMENTAL           1.00295305
//	crypto:SolvBTC_FUNDAMENTAL_USD      78313.02974310
//
// The two `_USD` legs are byte-identical (also true of an earlier
// capture, 6543063913439 both), which is what fixes each ratio's
// denominator:
//
//   - SolvBTC NAV in USD / BTC in USD = 1.00295 ⇒ the bare
//     `SolvBTC_FUNDAMENTAL` ratio is denominated in BTC.
//   - SolvBTC.BBN NAV in USD equals SolvBTC NAV in USD while the bare
//     ratio is exactly 1.00000000 (three independent captures:
//     lake ledger 60104689 and two live reads) ⇒ SolvBTC.BBN is
//     1:1 with SolvBTC and its ratio is denominated in SolvBTC, not
//     BTC. Quoting it crypto:BTC would contradict our own
//     `SolvBTC.BBN_FUNDAMENTAL_USD` row.
func TestDecode_SolvBTCFamily_NAVRatiosQuotedInReserveAsset(t *testing.T) {
	feedIDs := []string{
		"SolvBTC.BBN_FUNDAMENTAL",
		"SolvBTC.BBN_FUNDAMENTAL/USD",
		"SolvBTC_FUNDAMENTAL",
		"SolvBTC_FUNDAMENTAL/USD",
	}
	prices := []*big.Int{
		big.NewInt(1_00000000),     // 1.00000000 SolvBTC per SolvBTC.BBN
		big.NewInt(78313_02974310), // $78,313.02974310
		big.NewInt(1_00295305),     // 1.00295305 BTC per SolvBTC
		big.NewInt(78313_02974310), // $78,313.02974310
	}
	body := encodeWritePricesBody(t, relayerG, prices, 1_756_400_000_000, 1_756_400_060_000)
	ev := &events.Event{
		Topic: []string{TopicSymbolRedstone},
		Value: body,
		OpArgs: []string{
			encodeAddressArg(t, relayerG),
			encodeStringVecArg(t, feedIDs),
			encodePayloadArg(t),
		},
		TxHash: "abcd",
	}
	updates, err := decodeWritePrices(ev, time.Now())
	if err != nil {
		t.Fatalf("decodeWritePrices: %v", err)
	}
	if len(updates) != len(feedIDs) {
		t.Fatalf("got %d updates, want %d", len(updates), len(feedIDs))
	}

	want := []struct{ asset, quote string }{
		{"crypto:SolvBTC.BBN_FUNDAMENTAL", "crypto:SolvBTC"},
		{"crypto:SolvBTC.BBN_FUNDAMENTAL_USD", "fiat:USD"},
		{"crypto:SolvBTC_FUNDAMENTAL", "crypto:BTC"},
		{"crypto:SolvBTC_FUNDAMENTAL_USD", "fiat:USD"},
	}
	for i, w := range want {
		if got := updates[i].Asset.String(); got != w.asset {
			t.Errorf("feed %q → asset %s, want %s", feedIDs[i], got, w.asset)
		}
		if got := updates[i].Quote.String(); got != w.quote {
			t.Errorf("feed %q → quote %s, want %s (D8: a NAV ratio must name the asset it is "+
				"denominated in, never fiat:USD)", feedIDs[i], got, w.quote)
		}
		// The label is the only thing D8 got wrong — the observed
		// value must pass through byte-identical.
		if updates[i].Price.BigInt().Cmp(prices[i]) != 0 {
			t.Errorf("feed %q → price %s, want %s unchanged", feedIDs[i], updates[i].Price, prices[i])
		}
	}
}

// TestFeedRegistry_EarnUSDCIsItsOwnInstrument pins the identity of the
// Gami earnUSDC vault feed. A yield-bearing claim on USDC is NOT USDC:
// its value accrues away from the peg (observed 0.7176 -> 0.7200 over
// six days, monotonically), so resolving the two to one asset would
// publish a drifting number under the peg's identity. That is the
// asset-identity failure that put attacker-authored pricing on a served
// surface once already, which is why this is pinned rather than assumed.
//
// The base is the vault's Soroban contract because the vault is
// TOKENIZED — it mints its own shares, so the vault and the share token
// are one contract, and pricing it by contract id lands the oracle value
// on the same asset id as the deposits/withdrawals we already index.
func TestFeedRegistry_EarnUSDCIsItsOwnInstrument(t *testing.T) {
	entry, ok := feedRegistry["earnUSDC_FUNDAMENTAL"]
	if !ok {
		t.Fatal("earnUSDC_FUNDAMENTAL is not registered — it would fall back to raw: and be dropped from the price surface")
	}
	if entry.Base.Type != canonical.AssetSoroban {
		t.Errorf("base is %s (type %v), want the vault's Soroban contract id — a bare ticker cannot be joined to on-chain activity",
			entry.Base.String(), entry.Base.Type)
	}
	if entry.Base.String() == "" || !strings.Contains(entry.Base.String(), "CCL3WITW") {
		t.Errorf("base %q is not the verified earnUSDC vault contract (CCL3WITW…); the other address circulated for this vault has zero lake events",
			entry.Base.String())
	}
	// The whole point: distinct from USDC.
	if usdc, found := feedRegistry["USDC"]; found && usdc.Base.Equal(entry.Base) {
		t.Error("earnUSDC and USDC resolve to the SAME canonical asset — a yield-bearing vault share is not the stablecoin it is a claim on")
	}
	if entry.Invert {
		t.Error("earnUSDC_FUNDAMENTAL is published token-in-quote; Invert must be false")
	}
}

// ─── attribution-chain hardening ────────────────────────────
//
// These tests pin the decode-side layers that bind a set of attached op
// args to the event they claim to describe: duplicate-feed refusal, the
// body↔args updater cross-check, and the equal-arity state-write
// corroboration. The dispatcher/lake OpArgs provenance gate (args only
// from a direct call into the emitting contract) is pinned in
// internal/dispatcher/oparg_provenance_test.go and
// internal/storage/clickhouse/extract_events_gate_test.go.

// otherG is a second deterministic G-address, distinct from relayerG.
var otherG = func() string {
	var seed [32]byte
	for i := range seed {
		seed[i] = byte(0xF0 - i)
	}
	s, err := strkey.Encode(strkey.VersionByteAccountID, seed[:])
	if err != nil {
		panic("strkey encode of fixed seed failed: " + err.Error())
	}
	return s
}()

// adapterFeedKey builds the base64 XDR LedgerKey of the adapter's
// contract-data entry for one feed — the shape events.Event.
// StateWriteKeys carries (ContractData{adapter, ScString(feed)}).
func adapterFeedKey(t *testing.T, feed string) string {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, adapterC)
	if err != nil {
		t.Fatal(err)
	}
	var cid xdr.ContractId
	copy(cid[:], raw)
	skey := xdr.ScString(feed)
	lk := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &skey},
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	b64, err := xdr.MarshalBase64(lk)
	if err != nil {
		t.Fatal(err)
	}
	return b64
}

// hardeningEvent builds an equal-arity two-feed event (BTC, ETH) with
// consistent body/args updaters, ready for per-test mutation.
func hardeningEvent(t *testing.T, feedIDs []string) *events.Event {
	t.Helper()
	prices := make([]*big.Int, len(feedIDs))
	for i := range prices {
		prices[i] = big.NewInt(int64(1_000_000 * (i + 1)))
	}
	body := encodeWritePricesBody(t, relayerG, prices, 1_745_000_000_000, 1_745_000_060_000)
	return &events.Event{
		Topic:      []string{TopicSymbolRedstone},
		Value:      body,
		ContractID: adapterC,
		Ledger:     52_000_000,
		TxHash:     "hardening",
		OpArgs: []string{
			encodeAddressArg(t, relayerG),
			encodeStringVecArg(t, feedIDs),
			encodePayloadArg(t),
		},
	}
}

func TestDecode_DuplicateFeedIDs_Refused(t *testing.T) {
	// A genuine write_prices cannot carry duplicate feed_ids — the
	// redstone-core SDK's Config::try_new refuses them before the
	// adapter can emit (ConfigReoccurringFeedId). Duplicates were also
	// an attribution lever: a repeated feed inflated the state-write
	// subset arity into forcing the payload fallback.
	ev := hardeningEvent(t, []string{"BTC", "BTC"})
	_, err := decodeWritePrices(ev, time.Unix(1_745_000_000, 0))
	if !errors.Is(err, ErrDuplicateFeedIDs) {
		t.Fatalf("err = %v, want ErrDuplicateFeedIDs", err)
	}
}

func TestDecode_UpdaterMismatch_Refused(t *testing.T) {
	// write_prices publishes its own updater argument in the event
	// body; args whose args[0] names someone else did not drive this
	// event and must not supply its feed identities.
	ev := hardeningEvent(t, []string{"BTC", "ETH"})
	ev.OpArgs[0] = encodeAddressArg(t, otherG)
	_, err := decodeWritePrices(ev, time.Unix(1_745_000_000, 0))
	if !errors.Is(err, ErrUpdaterMismatch) {
		t.Fatalf("err = %v, want ErrUpdaterMismatch", err)
	}
}

func TestDecode_EqualArity_StateWriteCorroboration(t *testing.T) {
	base := func() *events.Event { return hardeningEvent(t, []string{"BTC", "ETH"}) }

	t.Run("matching set decodes (order-insensitive)", func(t *testing.T) {
		ev := base()
		ev.StateWriteKeys = []string{adapterFeedKey(t, "ETH"), adapterFeedKey(t, "BTC")}
		out, err := decodeWritePrices(ev, time.Unix(1_745_000_000, 0))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(out) != 2 {
			t.Fatalf("got %d updates, want 2", len(out))
		}
	})
	t.Run("foreign feed in written set refuses", func(t *testing.T) {
		// The op stored PYUSD+ETH but the args claim BTC+ETH: the
		// positional zip would stamp BTC's identity on a price the
		// contract stored under a different feed. Refuse.
		ev := base()
		ev.StateWriteKeys = []string{adapterFeedKey(t, "PYUSD"), adapterFeedKey(t, "ETH")}
		if _, err := decodeWritePrices(ev, time.Unix(1_745_000_000, 0)); !errors.Is(err, ErrStateWriteFeedMismatch) {
			t.Fatalf("err = %v, want ErrStateWriteFeedMismatch", err)
		}
	})
	t.Run("missing feed in written set refuses", func(t *testing.T) {
		// Equal arity claims every feed was accepted, but only one
		// entry actually changed — the claim is uncorroborated.
		ev := base()
		ev.StateWriteKeys = []string{adapterFeedKey(t, "BTC")}
		if _, err := decodeWritePrices(ev, time.Unix(1_745_000_000, 0)); !errors.Is(err, ErrStateWriteFeedMismatch) {
			t.Fatalf("err = %v, want ErrStateWriteFeedMismatch", err)
		}
	})
	t.Run("no keys plumbed: positional zip stands", func(t *testing.T) {
		// Absence is "unknown", not "no writes" — RPC fixtures and
		// non-opted readers keep the pre-hardening behaviour.
		ev := base()
		out, err := decodeWritePrices(ev, time.Unix(1_745_000_000, 0))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(out) != 2 {
			t.Fatalf("got %d updates, want 2", len(out))
		}
	})
}

// encodePayloadArgBytes is encodePayloadArg parameterized over the raw
// payload bytes, for tests that need a real (or deliberately malformed)
// RedStone payload rather than the two-byte sentinel.
func encodePayloadArgBytes(t *testing.T, raw []byte) string {
	t.Helper()
	b := xdr.ScBytes(raw)
	sv := xdr.ScVal{Type: xdr.ScValTypeScvBytes, Bytes: &b}
	marshaled, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return base64.StdEncoding.EncodeToString(marshaled)
}

func TestDecode_EqualArity_StateWriteMismatch_FallsBackToPayload(t *testing.T) {
	// hardeningEvent's BTC/ETH prices are 1_000_000 and 2_000_000 at
	// package_timestamp 1_745_000_000_000 (see hardeningEvent). Build a
	// real payload whose feed medians match those prices exactly: the
	// state-write claim disagrees (only BTC's key is reported changed),
	// but the payload independently and uniquely corroborates BOTH
	// feeds, so the fallback must attribute rather than refuse (the
	// degradation internal/dispatcher/state_write_keys.go documents).
	ev := hardeningEvent(t, []string{"BTC", "ETH"})
	payload := buildTestPayload(t, 1_745_000_000_000, map[string][]int64{
		"BTC": {1_000_000},
		"ETH": {2_000_000},
	})
	ev.OpArgs[2] = encodePayloadArgBytes(t, payload)
	ev.StateWriteKeys = []string{adapterFeedKey(t, "BTC")} // missing ETH's key: equal-arity mismatch

	out, err := decodeWritePrices(ev, time.Unix(1_745_000_000, 0))
	if err != nil {
		t.Fatalf("decode: %v, want fallback attribution via payload median", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d updates, want 2", len(out))
	}
	if out[0].Asset.String() != "crypto:BTC" || out[1].Asset.String() != "crypto:ETH" {
		t.Fatalf("attributed to %s/%s, want crypto:BTC then crypto:ETH", out[0].Asset, out[1].Asset)
	}
}

func TestSubsetFromStateWrites_DuplicateCandidatesCountWrittenFeedOnce(t *testing.T) {
	// Defence-in-depth below the ErrDuplicateFeedIDs gate: a repeated
	// candidate must not count the single written feed twice and
	// inflate the subset arity.
	key := adapterFeedKey(t, "BTC")
	sub := subsetFromStateWrites([]string{"BTC", "BTC", "ETH"}, []string{key}, adapterC)
	if len(sub) != 1 || sub[0] != "BTC" {
		t.Fatalf("sub = %v, want [BTC] exactly once", sub)
	}
}

// The wrapper-invoked shape after the dispatcher/lake provenance gate:
// a non-empty batch whose event arrived WITHOUT args (the op's
// top-level call targeted a wrapper, not the adapter) must refuse via
// ErrMissingOpArgs — honest-blind, counted as a decode error by the
// dispatcher — regardless of any state-write keys present.
func TestDecode_WrapperInvoked_NoArgs_RefusesHonestly(t *testing.T) {
	ev := hardeningEvent(t, []string{"BTC", "ETH"})
	ev.OpArgs = nil
	ev.StateWriteKeys = []string{adapterFeedKey(t, "BTC"), adapterFeedKey(t, "ETH")}
	_, err := decodeWritePrices(ev, time.Unix(1_745_000_000, 0))
	if !errors.Is(err, ErrMissingOpArgs) {
		t.Fatalf("err = %v, want ErrMissingOpArgs (no args → no feed identities → refuse, never guess)", err)
	}
}

// Compile-time-ish check that the Decoder declares its state-write
// interest to the dispatcher (the lazy enrichment gate keys off it).
func TestDecoder_DeclaresStateWriteContracts(t *testing.T) {
	d := NewDecoder(adapterC)
	got := d.StateWriteContracts()
	if len(got) != 1 || got[0] != adapterC {
		t.Fatalf("StateWriteContracts() = %v, want [%s]", got, adapterC)
	}
}

// ── Subset attribution from state-write keys (exact path) ──────────────
//
// Real lake fixture: the REDSTONE event at ledger 62,056,824 (tx
// 40758bde24a9…), captured from stellar.contract_events +
// stellar.ledger_entry_changes on r1. This is the FIRST of
// the 15 ledgers the payload-median rule provably cannot attribute: the
// op args request feed_ids [PYUSD, iBENJI_ETHEREUM_FUNDAMENTAL,
// BENJI_ETHEREUM_FUNDAMENTAL, USTRY], the adapter's freshness verifier
// kept exactly ONE feed, and the surviving price 100000000 (1.00000000
// at 8 decimals) equals BOTH BENJI twins' payload medians at the entry's
// package_timestamp — two order-preserving alignments, ErrAmbiguousSubset.
//
// The transaction's ledger-entry changes settle it: write_prices STORED
// exactly one PriceData under the adapter's contract-data key
// ScString("iBENJI_ETHEREUM_FUNDAMENTAL") (change_type=updated,
// change_index 5 — the change_index-4 `state` row is the pre-image and
// is excluded on both plumbing paths). With that key plumbed through
// events.Event.StateWriteKeys the accepted subset is exact and the
// event attributes with zero heuristics.

const stateWriteFixtureTx = "40758bde24a9afcfe5d4ed2e22fe166c3508566e38bb62619aaa2e86227c8842"

// stateWriteFixtureKey is the tx's single written adapter contract-data
// LedgerKey — ContractData{CA526Y2N…, ScString("iBENJI_ETHEREUM_FUNDAMENTAL"),
// persistent} — verbatim from stellar.ledger_entry_changes.key_xdr.
const stateWriteFixtureKey = "AAAABgAAAAE7r2NNhY1q1h+JSvMDLKe8+WE7oLTJ1BXmafczoXPOOwAAAA4AAAAbaUJFTkpJX0VUSEVSRVVNX0ZVTkRBTUVOVEFMAAAAAAE="

func stateWriteFixtureEvent() *events.Event {
	return &events.Event{
		ContractID: "CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG",
		Ledger:     62056824,
		TxHash:     stateWriteFixtureTx,
		Topic:      []string{TopicSymbolRedstone},
		Value:      "AAAADQAAAPgAAAARAAAAAQAAAAIAAAAPAAAADXVwZGF0ZWRfZmVlZHMAAAAAAAAQAAAAAQAAAAEAAAARAAAAAQAAAAMAAAAPAAAAEXBhY2thZ2VfdGltZXN0YW1wAAAAAAAABQAAAZ139fEgAAAADwAAAAVwcmljZQAAAAAAAAsAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABfXhAAAAAA8AAAAPd3JpdGVfdGltZXN0YW1wAAAAAAUAAAGdd/YcGAAAAA8AAAAHdXBkYXRlcgAAAAASAAAAAAAAAAAk1wP6EQQ6Z6YlFesVDZAkQ3o7tjIdDJoRh0/hHzC1Bw==",
		OpArgs: []string{
			"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",
		},
		StateWriteKeys: []string{stateWriteFixtureKey},
	}
}

func TestDecode_SubsetFilteredBatch_AttributedViaStateWriteKeys(t *testing.T) {
	out, err := decodeWritePrices(stateWriteFixtureEvent(), time.Unix(1775834111, 0))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d updates, want 1", len(out))
	}
	u := out[0]
	wantAsset, aerr := canonical.NewRWAAsset("iBENJI")
	if aerr != nil {
		t.Fatal(aerr)
	}
	if u.Asset != wantAsset {
		t.Errorf("attributed to %s, want %s (the written key names iBENJI; BENJI shares the median)", u.Asset, wantAsset)
	}
	if u.Price.String() != "100000000" {
		t.Errorf("price = %s, want 100000000", u.Price)
	}
	if got := u.Timestamp.UnixMilli(); got != 1775834100000 {
		t.Errorf("timestamp = %d, want package_timestamp 1775834100000", got)
	}
}

// Without the state-write keys the same event MUST stay honest-blind —
// pins that the exact path is what resolves it, and that the payload
// fallback's refusal semantics are unchanged.
func TestDecode_SubsetFilteredBatch_StillAmbiguousWithoutStateWriteKeys(t *testing.T) {
	ev := stateWriteFixtureEvent()
	ev.StateWriteKeys = nil
	_, err := decodeWritePrices(ev, time.Unix(1775834111, 0))
	if !errors.Is(err, ErrAmbiguousSubset) {
		t.Fatalf("err = %v, want ErrAmbiguousSubset (both BENJI twins share the surviving median)", err)
	}
}

// A written-key set whose feed intersection disagrees with updated_feeds'
// arity must NOT be trusted — the decoder falls back to payload-median
// alignment, which refuses this event as ambiguous. Keys can only force
// the fallback; they never stand in for it.
func TestDecode_SubsetFilteredBatch_ArityMismatchFallsBackToPayload(t *testing.T) {
	ev := stateWriteFixtureEvent()
	// Both BENJI twins' keys "written": intersection arity 2 != 1 price.
	ev.StateWriteKeys = []string{
		stateWriteFixtureKey,
		"AAAABgAAAAE7r2NNhY1q1h+JSvMDLKe8+WE7oLTJ1BXmafczoXPOOwAAAA4AAAAaQkVOSklfRVRIRVJFVU1fRlVOREFNRU5UQUwAAAAAAAE=",
	}
	_, err := decodeWritePrices(ev, time.Unix(1775834111, 0))
	if !errors.Is(err, ErrAmbiguousSubset) {
		t.Fatalf("err = %v, want ErrAmbiguousSubset via the payload fallback", err)
	}
}

// Keys owned by a DIFFERENT contract are ignored (defence-in-depth: the
// plumbing already filters by the event's contract).
func TestSubsetFromStateWrites_ForeignAndMalformedKeysIgnored(t *testing.T) {
	feedIDs := []string{"PYUSD", "iBENJI_ETHEREUM_FUNDAMENTAL", "BENJI_ETHEREUM_FUNDAMENTAL", "USTRY"}
	sub := subsetFromStateWrites(feedIDs, []string{
		"not-base64!!",       // unparseable — skipped
		stateWriteFixtureKey, // adapter's iBENJI key — kept
	}, "CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG")
	if len(sub) != 1 || sub[0] != "iBENJI_ETHEREUM_FUNDAMENTAL" {
		t.Fatalf("sub = %v, want [iBENJI_ETHEREUM_FUNDAMENTAL]", sub)
	}
	// Same key, wrong contract: contributes nothing.
	if sub := subsetFromStateWrites(feedIDs, []string{stateWriteFixtureKey}, "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"); sub != nil {
		t.Fatalf("foreign-contract sub = %v, want nil", sub)
	}
}

// ── Subset-filtered batch attribution (payload medians) ────────────────
//
// Real lake fixture: the REDSTONE event at ledger 59,258,375 (tx
// 1e9ddc61675fe641…), captured from stellar.contract_events.
// Its op args request feed_ids ["BTC","ETH"] but the
// adapter's freshness verifier dropped ETH, so updated_feeds carries ONE
// entry — the class that left 1,626 events honest-blind on the full
// completeness verify. The payload holds 3 signer packages per feed at
// timestamp 1,759,758,520,000; BTC's median is 12,449,969,251,710, which
// equals the surviving price byte-exactly, so attribution must yield
// exactly one BTC/USD update.

const subsetFixtureTx = "1e9ddc61675fe6419761781b68bc9ff8ef7cdb8be66df34d97b1c4bbe90897a4"

func subsetFixtureEvent() *events.Event {
	return &events.Event{
		ContractID: "CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG",
		Ledger:     59258375,
		TxHash:     subsetFixtureTx,
		Topic:      []string{TopicSymbolRedstone},
		Value:      "AAAADQAABJgAAAARAAAAAQAAAAMAAAAPAAAAB3BheWxvYWQAAAAADQAAA4dFVEgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAGs7lrw4AZm5yA7AAAAAIAAAAaVStYasO93sFR4XN3hj0Tqch8vL+PEAfPZ8z4bI4daKeGHe5Sz0UgZUwUAZ6Pd8+KnQ8mMc4m/foDSQUFNr5EgbRVRIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABrO51sAQGZucgOwAAAACAAAAGN2Q2jDaZZNaGuJ7MFv83lF7G+6QG2IBjrmlpqGcM641m+2AxQyp9Ztaunyf+PfmzrirNPqPPJH/gvhzXur/1KHEVUSAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAazuPGtIBmbnIDsAAAAAgAAAB4oTMFgY7HPnZcy7NGFFcVrsowRPcB/rjE0zR9gAhUq1qg91uimzCEQCTo7unVkY8FRyXvw0lANCBQycJpgkL0xtCVEMAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAC1K7/qV+AZm5yA7AAAAAIAAAAY1vKe63LkgwTwhXqVIQt6TpoEEL/z6S0RbtkpCKaHvnJUwVb4jP6pJKt0Y3bJ4PYh2X83uFUApWjRNO16p4MDkbQlRDAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAtSu/6lfgGZucgOwAAAACAAAAGA8R3odW/hVVVObKDzOE5yIGH5YX1foGyCnzwnP3GFLDs75iBv7lJ4hPrl8u7tcNn+Rq8veDNHmksLZULk0x5uG0JUQwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAALUrvBmk4BmbnIDsAAAAAgAAAB8bzqT8gZ897nnzSBJ2LTamOw+UHMAbkSB7gqqMNXTXhV8n8coNMwqn8nBZubYPevrh7gyugGMrirX+jwvpwrHhwABjE3NTk3NTg1MzE2MzEjMC45LjAjc3RlbGxhci1jb25uZWN0b3IAACUAAALtVwEeAAAAAAAADwAAAA11cGRhdGVkX2ZlZWRzAAAAAAAAEAAAAAEAAAABAAAAEQAAAAEAAAADAAAADwAAABFwYWNrYWdlX3RpbWVzdGFtcAAAAAAAAAUAAAGZucgOwAAAAA8AAAAFcHJpY2UAAAAAAAALAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAALUrv+pX4AAAAPAAAAD3dyaXRlX3RpbWVzdGFtcAAAAAAFAAABmbnITUAAAAAPAAAAB3VwZGF0ZXIAAAAAEgAAAAAAAAAA+pL1D+PjBeECGvNmk0i7fe2N6WSxRQLZP99ae7FY7Wk=",
		OpArgs:     []string{"AAAAEgAAAAAAAAAA+pL1D+PjBeECGvNmk0i7fe2N6WSxRQLZP99ae7FY7Wk=", "AAAAEAAAAAEAAAACAAAADgAAAANFVEgAAAAADgAAAANCVEMA", "AAAADQAAA4dFVEgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAGs7lrw4AZm5yA7AAAAAIAAAAaVStYasO93sFR4XN3hj0Tqch8vL+PEAfPZ8z4bI4daKeGHe5Sz0UgZUwUAZ6Pd8+KnQ8mMc4m/foDSQUFNr5EgbRVRIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABrO51sAQGZucgOwAAAACAAAAGN2Q2jDaZZNaGuJ7MFv83lF7G+6QG2IBjrmlpqGcM641m+2AxQyp9Ztaunyf+PfmzrirNPqPPJH/gvhzXur/1KHEVUSAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAazuPGtIBmbnIDsAAAAAgAAAB4oTMFgY7HPnZcy7NGFFcVrsowRPcB/rjE0zR9gAhUq1qg91uimzCEQCTo7unVkY8FRyXvw0lANCBQycJpgkL0xtCVEMAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAC1K7/qV+AZm5yA7AAAAAIAAAAY1vKe63LkgwTwhXqVIQt6TpoEEL/z6S0RbtkpCKaHvnJUwVb4jP6pJKt0Y3bJ4PYh2X83uFUApWjRNO16p4MDkbQlRDAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAtSu/6lfgGZucgOwAAAACAAAAGA8R3odW/hVVVObKDzOE5yIGH5YX1foGyCnzwnP3GFLDs75iBv7lJ4hPrl8u7tcNn+Rq8veDNHmksLZULk0x5uG0JUQwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAALUrvBmk4BmbnIDsAAAAAgAAAB8bzqT8gZ897nnzSBJ2LTamOw+UHMAbkSB7gqqMNXTXhV8n8coNMwqn8nBZubYPevrh7gyugGMrirX+jwvpwrHhwABjE3NTk3NTg1MzE2MzEjMC45LjAjc3RlbGxhci1jb25uZWN0b3IAACUAAALtVwEeAAAA"},
	}
}

func TestDecode_SubsetFilteredBatch_AttributedViaPayloadMedian(t *testing.T) {
	out, err := decodeWritePrices(subsetFixtureEvent(), time.Unix(1759758525, 0))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d updates, want 1", len(out))
	}
	u := out[0]
	if u.Asset.String() != "crypto:BTC" || u.Quote.String() != "fiat:USD" {
		t.Errorf("attributed to %s/%s, want crypto:BTC/fiat:USD (ETH was the dropped feed)", u.Asset, u.Quote)
	}
	if u.Price.String() != "12449969251710" {
		t.Errorf("price = %s, want 12449969251710 (BTC payload median)", u.Price)
	}
	if got := u.Timestamp.UnixMilli(); got != 1759758520000 {
		t.Errorf("timestamp = %d, want package_timestamp 1759758520000", got)
	}
}

func TestParsePayload_RealFixture(t *testing.T) {
	payload, err := payloadFromOpArgs(subsetFixtureEvent().OpArgs)
	if err != nil {
		t.Fatal(err)
	}
	byFeed, err := parsePayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(byFeed) != 2 || len(byFeed["BTC"]) != 3 || len(byFeed["ETH"]) != 3 {
		t.Fatalf("byFeed shape = %v", byFeed)
	}
	m, ok := medianAt(byFeed["BTC"], 1759758520000)
	if !ok || m.String() != "12449969251710" {
		t.Errorf("BTC median = %v ok=%v, want 12449969251710", m, ok)
	}
	m, ok = medianAt(byFeed["ETH"], 1759758520000)
	if !ok || m.String() != "460561235000" {
		t.Errorf("ETH median = %v ok=%v, want 460561235000", m, ok)
	}
	if _, ok := medianAt(byFeed["BTC"], 42); ok {
		t.Error("medianAt with a non-matching timestamp must report ok=false")
	}
}

// buildTestPayload constructs a syntactically-valid RedStone payload:
// one package per (feed, value) pair, all at timestamp ts.
func buildTestPayload(t *testing.T, ts uint64, points map[string][]int64) []byte {
	t.Helper()
	var out []byte
	pkgs := 0
	for feed, vals := range points {
		for _, v := range vals {
			// one data point: feedID(32) + value(8)
			var dp [40]byte
			copy(dp[:32], feed)
			binary.BigEndian.PutUint64(dp[32:], uint64(v))
			pkg := append([]byte{}, dp[:]...)
			var ts6 [6]byte
			ts6[0] = byte(ts >> 40)
			ts6[1] = byte(ts >> 32)
			binary.BigEndian.PutUint32(ts6[2:], uint32(ts))
			pkg = append(pkg, ts6[:]...)
			var vs [4]byte
			binary.BigEndian.PutUint32(vs[:], 8)
			pkg = append(pkg, vs[:]...)
			pkg = append(pkg, 0, 0, 1) // dpCount=1
			pkg = append(pkg, make([]byte, 65)...)
			out = append(out, pkg...)
			pkgs++
		}
	}
	var cnt [2]byte
	binary.BigEndian.PutUint16(cnt[:], uint16(pkgs))
	out = append(out, cnt[:]...)
	out = append(out, 0, 0, 0) // unsignedMetadataSize = 0
	out = append(out, redstoneMarker...)
	return out
}

func pd(t *testing.T, price int64, ts uint64) priceDataDecoded {
	t.Helper()
	return priceDataDecoded{Price: amountFromInt64(t, price), PackageTimestamp: ts}
}

func TestAttributeSubset_Adversarial(t *testing.T) {
	const ts = uint64(1700000000000)
	payload := buildTestPayload(t, ts, map[string][]int64{
		"BTC":  {100, 101, 102},
		"USDC": {1_0000_0000},
		"USDT": {1_0000_0000},
	})

	t.Run("unique match attributes", func(t *testing.T) {
		got, err := attributeSubset([]priceDataDecoded{pd(t, 101, ts)}, []string{"BTC", "USDC"}, payload)
		if err != nil || len(got) != 1 || got[0] != "BTC" {
			t.Fatalf("got %v err=%v, want [BTC]", got, err)
		}
	})
	t.Run("two identical-median candidates refuse", func(t *testing.T) {
		_, err := attributeSubset([]priceDataDecoded{pd(t, 1_0000_0000, ts)}, []string{"USDC", "USDT"}, payload)
		if !errors.Is(err, ErrAmbiguousSubset) {
			t.Fatalf("err = %v, want ErrAmbiguousSubset", err)
		}
	})
	t.Run("no matching candidate refuses", func(t *testing.T) {
		_, err := attributeSubset([]priceDataDecoded{pd(t, 999, ts)}, []string{"BTC"}, payload)
		if !errors.Is(err, ErrAmbiguousSubset) {
			t.Fatalf("err = %v, want ErrAmbiguousSubset", err)
		}
	})
	t.Run("bijection violation refuses", func(t *testing.T) {
		_, err := attributeSubset([]priceDataDecoded{pd(t, 101, ts), pd(t, 101, ts)}, []string{"BTC", "USDC"}, payload)
		if !errors.Is(err, ErrAmbiguousSubset) {
			t.Fatalf("err = %v, want ErrAmbiguousSubset", err)
		}
	})
	t.Run("timestamp mismatch refuses", func(t *testing.T) {
		_, err := attributeSubset([]priceDataDecoded{pd(t, 101, ts+1)}, []string{"BTC"}, payload)
		if !errors.Is(err, ErrAmbiguousSubset) {
			t.Fatalf("err = %v, want ErrAmbiguousSubset", err)
		}
	})
	t.Run("malformed payload refuses", func(t *testing.T) {
		_, err := attributeSubset([]priceDataDecoded{pd(t, 101, ts)}, []string{"BTC"}, []byte("garbage"))
		if !errors.Is(err, ErrMalformedRedstonePayload) {
			t.Fatalf("err = %v, want ErrMalformedRedstonePayload", err)
		}
	})
}

func TestMedianAt_EvenCountTruncatedMean(t *testing.T) {
	pkgs := []payloadPackage{
		{Value: big.NewInt(10), TimestampMS: 1},
		{Value: big.NewInt(13), TimestampMS: 1},
	}
	m, ok := medianAt(pkgs, 1)
	if !ok || m.Int64() != 11 { // (10+13)/2 truncated
		t.Fatalf("even median = %v ok=%v, want 11", m, ok)
	}
}

func amountFromInt64(t *testing.T, v int64) canonical.Amount {
	t.Helper()
	return canonical.NewAmount(big.NewInt(v))
}

// ── Order-preserving alignment (the residual class) ────────

// Real lake fixture: ledger 60104689's subset batch (7 feed_ids →
// 5 updated_feeds) where one price matches TWO candidates' medians
// (iBENJI_ETHEREUM_FUNDAMENTAL and SolvBTC.BBN_FUNDAMENTAL) — refused by
// the unordered rule, uniquely resolved by the order-preserving
// subsequence alignment (the adapter builds updated_feeds in one pass
// over feed_ids, so order is guaranteed).
func TestDecode_OrderPreservingAlignment_DisambiguatesSharedMedians(t *testing.T) {
	e := &events.Event{
		ContractID: "CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG",
		Ledger:     60104689,
		TxHash:     "3333e95cc53cfe9c3fec32c8455d9c4c189b625dd34c66cb3bb4fcfa822e5bfc",
		Topic:      []string{TopicSymbolRedstone},
		Value:      "AAAADQAAAygAAAARAAAAAQAAAAIAAAAPAAAADXVwZGF0ZWRfZmVlZHMAAAAAAAAQAAAAAQAAAAUAAAARAAAAAQAAAAMAAAAPAAAAEXBhY2thZ2VfdGltZXN0YW1wAAAAAAAABQAAAZra6xHwAAAADwAAAAVwcmljZQAAAAAAAAsAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABfUdCwAAAA8AAAAPd3JpdGVfdGltZXN0YW1wAAAAAAUAAAGa2utQcAAAABEAAAABAAAAAwAAAA8AAAARcGFja2FnZV90aW1lc3RhbXAAAAAAAAAFAAABmtrrEfAAAAAPAAAABXByaWNlAAAAAAAACwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAF9a/ZAAAADwAAAA93cml0ZV90aW1lc3RhbXAAAAAABQAAAZra61BwAAAAEQAAAAEAAAADAAAADwAAABFwYWNrYWdlX3RpbWVzdGFtcAAAAAAAAAUAAAGa2usR8AAAAA8AAAAFcHJpY2UAAAAAAAALAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAX2ArsAAAAPAAAAD3dyaXRlX3RpbWVzdGFtcAAAAAAFAAABmtrrUHAAAAARAAAAAQAAAAMAAAAPAAAAEXBhY2thZ2VfdGltZXN0YW1wAAAAAAAABQAAAZra6xHwAAAADwAAAAVwcmljZQAAAAAAAAsAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABfXhAAAAAA8AAAAPd3JpdGVfdGltZXN0YW1wAAAAAAUAAAGa2utQcAAAABEAAAABAAAAAwAAAA8AAAARcGFja2FnZV90aW1lc3RhbXAAAAAAAAAFAAABmtrrEfAAAAAPAAAABXByaWNlAAAAAAAACwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAF9eEAAAAADwAAAA93cml0ZV90aW1lc3RhbXAAAAAABQAAAZra61BwAAAADwAAAAd1cGRhdGVyAAAAABIAAAAAAAAAACOeYtRzBVeWeEd+3c4Ep57O6iY28c8Orgh5M25m+l0S",
		OpArgs:     []string{"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"},
	}
	out, err := decodeWritePrices(e, time.Unix(1750000000, 0))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 5 {
		t.Fatalf("got %d updates, want 5 (7 requested, 2 freshness-dropped)", len(out))
	}
	// Pin the exact attribution, not just the count: the unique
	// order-preserving alignment drops ETH and BTC (the first two
	// candidates) and maps the five surviving prices onto the
	// remaining feed_ids IN ORDER. A regression that found a different
	// (wrong) unique alignment would still pass a len-only assertion.
	wantPairs := []struct{ asset, quote string }{
		{"crypto:PYUSD", "fiat:USD"},
		{"crypto:USDC", "fiat:USD"},
		{"crypto:EUROC", "fiat:EUR"}, // the EUROC/EUR feed — EUR-quoted
		{"rwa:iBENJI", "fiat:USD"},
		// Quote: this fixture's price is
		// exactly 1.00000000, a NAV ratio against SolvBTC — not a
		// $1.00 price. See feeds.go for the derivation.
		{"crypto:SolvBTC.BBN_FUNDAMENTAL", "crypto:SolvBTC"},
	}
	for i, want := range wantPairs {
		if got := out[i].Asset.String(); got != want.asset {
			t.Errorf("out[%d].Asset = %s, want %s", i, got, want.asset)
		}
		if got := out[i].Quote.String(); got != want.quote {
			t.Errorf("out[%d].Quote = %s, want %s", i, got, want.quote)
		}
	}
}

func TestAttributeSubset_OrderDisambiguates(t *testing.T) {
	// Two prices each matching BOTH candidates' medians: unordered
	// attribution is ambiguous, but only ONE order-preserving complete
	// alignment exists (the diagonal), so it attributes [A, B].
	const ts = uint64(1700000000000)
	payload := buildTestPayload(t, ts, map[string][]int64{
		"USDA": {100},
		"USDB": {100},
	})
	got, err := attributeSubset([]priceDataDecoded{pd(t, 100, ts), pd(t, 100, ts)}, []string{"USDA", "USDB"}, payload)
	if err != nil || len(got) != 2 || got[0] != "USDA" || got[1] != "USDB" {
		t.Fatalf("got %v err=%v, want [USDA USDB]", got, err)
	}
	// One price, two identical-median candidates: STILL ambiguous under
	// order (two alignments) — must refuse.
	if _, err := attributeSubset([]priceDataDecoded{pd(t, 100, ts)}, []string{"USDA", "USDB"}, payload); !errors.Is(err, ErrAmbiguousSubset) {
		t.Fatalf("err = %v, want ErrAmbiguousSubset", err)
	}
}

// ─── an unrepresentable ScString feed_id must not black out the batch ──
//
// RedStone feed_ids arrive as `ScString` — arbitrary bytes, unbounded
// length — not the `ScSymbol` the Reflector/Band raw path was written
// against. So `canonical.NewOracleRawAsset` CAN refuse one, and because
// write_prices batches every updated feed into ONE event, an
// event-level refusal takes every feed dark until a code change.
// Refusal must therefore be per-SLOT: the unrepresentable slot is
// dropped (counted + WARN-logged), every sibling feed still lands, and
// op_index positions are unchanged.

// oneUnrepresentableFeedID returns the args + body for a three-feed
// batch whose middle feed_id is `bad`.
func threeFeedBatch(t *testing.T, middle string) *events.Event {
	t.Helper()
	body := encodeWritePricesBody(t, relayerG,
		[]*big.Int{
			big.NewInt(oneBTCAt8),
			big.NewInt(9_000_000),
			big.NewInt(oneETHAt8),
		}, 1, 2)
	return &events.Event{
		Topic: []string{TopicSymbolRedstone},
		Value: body,
		OpArgs: []string{
			encodeAddressArg(t, relayerG),
			encodeStringVecArg(t, []string{"BTC", middle, "ETH"}),
			encodePayloadArg(t),
		},
		ContractID: adapterC,
		Ledger:     63624934,
		TxHash:     "abcd",
	}
}

func TestDecode_UnrepresentableFeedID_SkipsSlotNotEvent(t *testing.T) {
	cases := []struct {
		name   string
		feedID string
	}{
		// A control byte: legal in an ScString, refused by the raw
		// validator's printable-ASCII charset.
		{"control byte", "BAD\x01FEED"},
		// Non-ASCII (UTF-8) — same class.
		{"non-ascii", "BTCü"},
		// Longer than the 64-byte raw cap.
		{"over 64 bytes", strings.Repeat("A", 65)},
		// Whitespace is outside 0x21-0x7E too.
		{"embedded space", "HAS SPACE"},
		// Empty string.
		{"empty", ""},
	}
	btc, _ := canonical.NewCryptoAsset("BTC")
	eth, _ := canonical.NewCryptoAsset("ETH")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Pre-flight: the case is only meaningful while the raw
			// validator genuinely refuses this feed_id.
			if _, err := canonical.NewOracleRawAsset(tc.feedID); err == nil {
				t.Fatalf("fixture %q is representable — pick another", tc.feedID)
			}
			ev := threeFeedBatch(t, tc.feedID)

			dropped := obs.SourceUnrepresentableSymbolsTotal.WithLabelValues(SourceName)
			before := testutil.ToFloat64(dropped)

			updates, err := decodeWritePrices(ev, time.Now())
			if err != nil {
				t.Fatalf("one unrepresentable feed_id must not fail the whole event: %v", err)
			}
			if len(updates) != 2 {
				t.Fatalf("expected 2 updates (BTC + ETH survive), got %d", len(updates))
			}
			if !updates[0].Asset.Equal(btc) {
				t.Errorf("updates[0].Asset = %s, want %s", updates[0].Asset, btc)
			}
			if !updates[1].Asset.Equal(eth) {
				t.Errorf("updates[1].Asset = %s, want %s", updates[1].Asset, eth)
			}
			// Slot stability: the surviving rows keep their ORIGINAL vector
			// slots — ETH stays at 2, it does not slide into the
			// dropped slot 1.
			if updates[0].OpIndex != 0 {
				t.Errorf("BTC OpIndex = %d, want 0", updates[0].OpIndex)
			}
			if updates[1].OpIndex != 2 {
				t.Errorf("ETH OpIndex = %d, want 2 (dropped slot 1 must not shift it)", updates[1].OpIndex)
			}
			if got := testutil.ToFloat64(dropped) - before; got != 1 {
				t.Errorf("stellarindex_source_unrepresentable_symbols_total{source=redstone} rose by %v, want 1", got)
			}
		})
	}
}

// The drop is NOT counted as an unknown-but-recorded symbol: that
// counter's contract is "recorded verbatim as raw:<symbol>", and this
// slot was recorded nowhere. Conflating them would send the operator
// hunting for raw rows that do not exist.
func TestDecode_UnrepresentableFeedID_NotCountedAsRawRecorded(t *testing.T) {
	ev := threeFeedBatch(t, "BAD\x01FEED")
	recorded := obs.SourceUnknownSymbolsTotal.WithLabelValues(SourceName)
	before := testutil.ToFloat64(recorded)
	if _, err := decodeWritePrices(ev, time.Now()); err != nil {
		t.Fatalf("decodeWritePrices: %v", err)
	}
	if got := testutil.ToFloat64(recorded) - before; got != 0 {
		t.Errorf("stellarindex_source_unknown_symbols_total{source=redstone} rose by %v, want 0 (nothing was recorded as raw:)", got)
	}
}

// A batch in which EVERY feed_id is unrepresentable records nothing,
// so it stays an honest decode error (ErrEmptyUpdates) rather than
// silently projecting an empty batch as a no-op.
func TestDecode_AllUnrepresentable_IsEmptyUpdates(t *testing.T) {
	body := encodeWritePricesBody(t, relayerG,
		[]*big.Int{big.NewInt(1), big.NewInt(2)}, 1, 2)
	ev := &events.Event{
		Topic: []string{TopicSymbolRedstone},
		Value: body,
		OpArgs: []string{
			encodeAddressArg(t, relayerG),
			encodeStringVecArg(t, []string{"BAD\x01ONE", "BAD\x02TWO"}),
			encodePayloadArg(t),
		},
		ContractID: adapterC,
		TxHash:     "abcd",
	}
	updates, err := decodeWritePrices(ev, time.Now())
	if !errors.Is(err, ErrEmptyUpdates) {
		t.Fatalf("err = %v, want ErrEmptyUpdates", err)
	}
	if len(updates) != 0 {
		t.Errorf("expected 0 updates, got %d", len(updates))
	}
}

// TestFeedRegistry_CountMatchesProtocolPages extends the doc-comment pin
// to the public protocol pages, which stated three different wrong counts
// after feeds were added.
func TestFeedRegistry_CountMatchesProtocolPages(t *testing.T) {
	pages := map[string][]*regexp.Regexp{
		"../../../docs/protocols/README.md": {
			regexp.MustCompile(`Adapter contract \+ (\d+)-feed registry`),
		},
		"../../../docs/protocols/redstone.md": {
			regexp.MustCompile(`Adapter contract and the (\d+)-feed`),
			regexp.MustCompile(`registry of the (\d+) mainnet`),
			regexp.MustCompile(`holds all (\d+) mainnet feeds`),
		},
	}
	for path, res := range pages {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, re := range res {
			m := re.FindStringSubmatch(string(body))
			if m == nil {
				t.Errorf("%s: no match for %q — the sentence moved; update this test with it", path, re)
				continue
			}
			if n, _ := strconv.Atoi(m[1]); n != len(feedRegistry) {
				t.Errorf("%s: %q says %d feeds, feedRegistry has %d", path, m[0], n, len(feedRegistry))
			}
		}
	}
}

// ─── consumer.go ──────────────────────────────────────────────────

func TestUpdateEvent_implementsConsumerEvent(t *testing.T) {
	ue := UpdateEvent{}
	if got := ue.EventKind(); got != "redstone.update" {
		t.Errorf("EventKind() = %q, want \"redstone.update\"", got)
	}
	if got := ue.Source(); got != SourceName {
		t.Errorf("Source() = %q, want %q", got, SourceName)
	}
	var _ consumer.Event = ue
}

// ─── dispatcher_adapter.go ────────────────────────────────────────

func TestDecoder_Name(t *testing.T) {
	if got := NewDecoder(adapterC).Name(); got != SourceName {
		t.Errorf("Name() = %q, want %q", got, SourceName)
	}
}

func TestDecoder_Matches(t *testing.T) {
	d := NewDecoder(adapterC)

	good := events.Event{
		Topic:      []string{TopicSymbolRedstone},
		ContractID: adapterC,
	}
	if !d.Matches(good) {
		t.Error("Matches(REDSTONE event from adapter) = false, want true")
	}

	wrongTopic := events.Event{
		Topic:      []string{"AAAACwAAAAhTT1JPU1dBUAAAAAA="},
		ContractID: adapterC,
	}
	if d.Matches(wrongTopic) {
		t.Error("Matches(non-REDSTONE topic) = true, want false")
	}

	wrongContract := events.Event{
		Topic:      []string{TopicSymbolRedstone},
		ContractID: "CWRONGADDRESS3333333333333333333333333333333333333333333",
	}
	if d.Matches(wrongContract) {
		t.Error("Matches(REDSTONE topic but wrong contract) = true, want false")
	}

	emptyTopic := events.Event{Topic: nil, ContractID: adapterC}
	if d.Matches(emptyTopic) {
		t.Error("Matches(empty topic) = true, want false")
	}
}

// Decoder.Decode wraps decodeWritePrices and threads its updates
// through the consumer.Event boundary. The interesting edges:
// happy-path emission, fallback to time.Now() on a malformed
// LedgerClosedAt, and propagation of decode errors.

func TestDecoder_Decode_happyPath(t *testing.T) {
	const pkgTs, wrTs = uint64(1_745_000_000_000), uint64(1_745_000_060_000)
	body := encodeWritePricesBody(t, relayerG,
		[]*big.Int{big.NewInt(oneBTCAt8), big.NewInt(oneETHAt8)},
		pkgTs, wrTs)
	args := []string{
		encodeAddressArg(t, relayerG),
		encodeStringVecArg(t, []string{"BTC", "ETH"}),
		encodePayloadArg(t),
	}

	ev := events.Event{
		Topic:          []string{TopicSymbolRedstone},
		Value:          body,
		OpArgs:         args,
		ContractID:     adapterC,
		Ledger:         52_000_000,
		TxHash:         "abcd",
		OperationIndex: 0,
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	}

	out, err := NewDecoder(adapterC).Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d events, want 2", len(out))
	}
	for i, e := range out {
		ue, ok := e.(UpdateEvent)
		if !ok {
			t.Fatalf("out[%d] not UpdateEvent: %T", i, e)
		}
		if ue.Update.Observer != relayerG {
			t.Errorf("out[%d].Observer = %q, want %q", i, ue.Update.Observer, relayerG)
		}
	}
}

func TestDecoder_Decode_malformedLedgerClosedAtFailsClosed(t *testing.T) {
	// LedgerClosedAt empty/invalid → EventClosedAt errors → adapter
	// FAILS CLOSED (returns the error) rather than substituting
	// time.Now(). closedAt is pickTimestamp's fallback when a
	// PriceData's PackageTimestamp is 0 / out of its sanity window, so
	// a wall-clock value here would mis-timestamp the row during a
	// backfill replay. Matches the comet/blend/phoenix siblings.
	body := encodeWritePricesBody(t, relayerG,
		[]*big.Int{big.NewInt(oneBTCAt8)}, 1, 2)
	args := []string{
		encodeAddressArg(t, relayerG),
		encodeStringVecArg(t, []string{"BTC"}),
		encodePayloadArg(t),
	}
	ev := events.Event{
		Topic:          []string{TopicSymbolRedstone},
		Value:          body,
		OpArgs:         args,
		ContractID:     adapterC,
		LedgerClosedAt: "", // triggers the fail-closed branch
	}

	out, err := NewDecoder(adapterC).Decode(ev)
	if err == nil {
		t.Fatalf("Decode with empty LedgerClosedAt should error, got nil (out=%v)", out)
	}
	if out != nil {
		t.Errorf("expected nil events on error, got %v", out)
	}
}

func TestDecoder_Decode_propagatesDecodeError(t *testing.T) {
	// 2 prices but 1 feed id — decodeWritePrices returns
	// ErrFeedIDCountMismatch. Decode must surface the error rather
	// than drop the event silently.
	body := encodeWritePricesBody(t, relayerG,
		[]*big.Int{big.NewInt(oneBTCAt8), big.NewInt(oneETHAt8)}, 1, 2)
	args := []string{
		encodeAddressArg(t, relayerG),
		encodeStringVecArg(t, []string{"BTC"}),
		encodePayloadArg(t),
	}
	ev := events.Event{
		Topic:          []string{TopicSymbolRedstone},
		Value:          body,
		OpArgs:         args,
		ContractID:     adapterC,
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	}

	if _, err := NewDecoder(adapterC).Decode(ev); err == nil {
		t.Error("expected error from feed-id count mismatch, got nil")
	}
}
