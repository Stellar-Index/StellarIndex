package redstone

import (
	"encoding/base64"
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

// encodeAddressArg builds the base64 SCVal::Address form of the
// relayer G-strkey — what the dispatcher would hand us in OpArgs[0].
func encodeAddressArg(t *testing.T, gStrkey string) string {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteAccountID, gStrkey)
	if err != nil {
		t.Fatalf("decode strkey: %v", err)
	}
	var pub xdr.Uint256
	copy(pub[:], raw)
	aid := xdr.AccountId{
		Type:    xdr.PublicKeyTypePublicKeyTypeEd25519,
		Ed25519: &pub,
	}
	addr := xdr.ScAddress{
		Type:      xdr.ScAddressTypeScAddressTypeAccount,
		AccountId: &aid,
	}
	sv := xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// encodeStringVecArg builds the base64 SCVal::Vec<String> that the
// write_prices op arg feed_ids comes in as.
func encodeStringVecArg(t *testing.T, feedIDs []string) string {
	t.Helper()
	items := make([]xdr.ScVal, len(feedIDs))
	for i, id := range feedIDs {
		s := xdr.ScString(id)
		items[i] = xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &s}
	}
	vec := xdr.ScVec(items)
	pvec := &vec
	sv := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pvec}
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal vec: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// encodePayloadArg builds the base64 ScBytes we use as args[2]; the
// decoder doesn't inspect it, so a short sentinel is enough.
func encodePayloadArg(t *testing.T) string {
	t.Helper()
	b := xdr.ScBytes{0xAA, 0xBB}
	sv := xdr.ScVal{Type: xdr.ScValTypeScvBytes, Bytes: &b}
	raw, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// encodeWritePricesBody builds the WritePrices event body:
//
//	Map { "updater": Address, "updated_feeds": Vec<PriceData> }
//
// prices are passed as *big.Int so tests can exercise the U256
// path (Redstone's canonical scale is 8 decimals; 1.0 at 8 dec = 1e8).
func encodeWritePricesBody(t *testing.T, updater string, prices []*big.Int, packageTs, writeTs uint64) string {
	t.Helper()
	// updater Address
	raw, err := strkey.Decode(strkey.VersionByteAccountID, updater)
	if err != nil {
		t.Fatalf("decode updater: %v", err)
	}
	var pub xdr.Uint256
	copy(pub[:], raw)
	aid := xdr.AccountId{
		Type:    xdr.PublicKeyTypePublicKeyTypeEd25519,
		Ed25519: &pub,
	}
	addr := xdr.ScAddress{
		Type:      xdr.ScAddressTypeScAddressTypeAccount,
		AccountId: &aid,
	}
	updaterSv := xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}

	// Vec<PriceData>
	items := make([]xdr.ScVal, len(prices))
	for i, p := range prices {
		priceSv := u256ScVal(t, p)
		pkgU := xdr.Uint64(packageTs)
		wrU := xdr.Uint64(writeTs)
		pkgSv := xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &pkgU}
		wrSv := xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &wrU}

		pdKeys := []string{"price", "package_timestamp", "write_timestamp"}
		pdVals := []xdr.ScVal{priceSv, pkgSv, wrSv}
		m := make(xdr.ScMap, len(pdKeys))
		for j, k := range pdKeys {
			sym := xdr.ScSymbol(k)
			m[j] = xdr.ScMapEntry{
				Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
				Val: pdVals[j],
			}
		}
		pm := &m
		items[i] = xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &pm}
	}
	vec := xdr.ScVec(items)
	pvec := &vec
	feedsSv := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pvec}

	// outer map
	outerKeys := []string{"updated_feeds", "updater"}
	outerVals := []xdr.ScVal{feedsSv, updaterSv}
	outer := make(xdr.ScMap, len(outerKeys))
	for i, k := range outerKeys {
		sym := xdr.ScSymbol(k)
		outer[i] = xdr.ScMapEntry{
			Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
			Val: outerVals[i],
		}
	}
	pouter := &outer
	body := xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &pouter}
	b, err := body.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// u256ScVal builds an ScVal::U256 from a non-negative *big.Int,
// splitting into the four 64-bit words the SDK expects.
func u256ScVal(t *testing.T, n *big.Int) xdr.ScVal {
	t.Helper()
	if n.Sign() < 0 {
		t.Fatalf("u256 does not accept negative: %s", n)
	}
	buf := n.Bytes() // big-endian, no leading zeros
	if len(buf) > 32 {
		t.Fatalf("value exceeds 256 bits: %s", n)
	}
	padded := make([]byte, 32)
	copy(padded[32-len(buf):], buf)
	hiHi := beUint64(padded[0:8])
	hiLo := beUint64(padded[8:16])
	loHi := beUint64(padded[16:24])
	loLo := beUint64(padded[24:32])
	parts := xdr.UInt256Parts{
		HiHi: xdr.Uint64(hiHi),
		HiLo: xdr.Uint64(hiLo),
		LoHi: xdr.Uint64(loHi),
		LoLo: xdr.Uint64(loLo),
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

// Deterministic test identifiers. `relayerG` is strkey-encoded
// from a fixed byte pattern so the checksum round-trips through
// strkey.Decode/Encode without depending on key generation.
// adapterC is the real mainnet adapter from
// docs/discovery/oracles/redstone.md.
const (
	adapterC  = "CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG"
	oneBTCAt8 = 50_000_000_000_000 // $500,000 at 8 decimals
	oneETHAt8 = 3_500_000_000_000  // $35,000 at 8 decimals
)

// relayerG is computed at init from a fixed 32-byte seed — a known
// public key bit pattern strkey-encodes to a valid G-address with a
// correct checksum. Hardcoding the strkey string invites checksum
// drift (what caught this test-suite the first time).
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

// ─── Tests ───────────────────────────────────────────────────────

func TestClassify_MatchesRedstone(t *testing.T) {
	e := &events.Event{Topic: []string{TopicSymbolRedstone}}
	if !classify(e) {
		t.Errorf("expected classify true for REDSTONE topic")
	}
	// non-matching
	e2 := &events.Event{Topic: []string{"AAAACwAAAAhTT1JPU1dBUAAAAAA="}}
	if classify(e2) {
		t.Errorf("expected classify false for non-REDSTONE")
	}
}

func TestDecode_HappyPath_TwoKnownFeeds(t *testing.T) {
	const pkgTs = uint64(1_745_000_000_000) // ms
	const wrTs = uint64(1_745_000_060_000)
	body := encodeWritePricesBody(t, relayerG,
		[]*big.Int{big.NewInt(oneBTCAt8), big.NewInt(oneETHAt8)},
		pkgTs, wrTs)

	args := []string{
		encodeAddressArg(t, relayerG),
		encodeStringVecArg(t, []string{"BTC", "ETH"}),
		encodePayloadArg(t),
	}

	ev := &events.Event{
		Topic:          []string{TopicSymbolRedstone},
		Value:          body,
		OpArgs:         args,
		ContractID:     adapterC,
		Ledger:         52_000_000,
		TxHash:         "abcd",
		OperationIndex: 0,
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	}
	closedAt, _ := time.Parse(time.RFC3339, ev.LedgerClosedAt)

	updates, err := decodeWritePrices(ev, closedAt)
	if err != nil {
		t.Fatalf("decodeWritePrices: %v", err)
	}
	if len(updates) != 2 {
		t.Fatalf("expected 2 updates, got %d", len(updates))
	}

	btc, _ := canonical.NewCryptoAsset("BTC")
	if !updates[0].Asset.Equal(btc) {
		t.Errorf("updates[0].Asset = %+v want %+v", updates[0].Asset, btc)
	}
	if updates[0].Price.BigInt().Cmp(big.NewInt(oneBTCAt8)) != 0 {
		t.Errorf("updates[0].Price = %s want %d", updates[0].Price, oneBTCAt8)
	}
	if updates[0].Decimals != 8 {
		t.Errorf("decimals = %d want 8", updates[0].Decimals)
	}
	// Timestamp should come from PackageTimestamp, not ledger close.
	if updates[0].Timestamp.UnixMilli() != int64(pkgTs) {
		t.Errorf("timestamp not from package_timestamp: got %d want %d",
			updates[0].Timestamp.UnixMilli(), pkgTs)
	}
	if updates[0].Observer != relayerG {
		t.Errorf("observer = %q want %q", updates[0].Observer, relayerG)
	}
	// OpIndex fan-out: same OperationIndex=0, slots 0 and 1.
	if updates[0].OpIndex != 0 || updates[1].OpIndex != 1 {
		t.Errorf("OpIndex fanout wrong: [%d, %d]", updates[0].OpIndex, updates[1].OpIndex)
	}

	eth, _ := canonical.NewCryptoAsset("ETH")
	if !updates[1].Asset.Equal(eth) {
		t.Errorf("updates[1].Asset = %+v want %+v", updates[1].Asset, eth)
	}
}

// TestDecodeWritePrices_EventIndexPreventsSameOpCollision is the
// regression test for DAT-06/trap-15 (audit-2026-07-23): two REDSTONE
// events emitted by the SAME operation (OperationIndex equal) but at
// different positions in that operation's contract-event list
// (EventIndex differs) used to collide, because the fanout base was
// OperationIndex ALONE. The fix must incorporate EventIndex too, so
// two events within one op get disjoint 1024-wide OpIndex blocks.
func TestDecodeWritePrices_EventIndexPreventsSameOpCollision(t *testing.T) {
	const pkgTs = uint64(1_745_000_000_000)
	const wrTs = uint64(1_745_000_060_000)
	body := encodeWritePricesBody(t, relayerG, []*big.Int{big.NewInt(oneBTCAt8)}, pkgTs, wrTs)
	args := []string{
		encodeAddressArg(t, relayerG),
		encodeStringVecArg(t, []string{"BTC"}),
		encodePayloadArg(t),
	}

	evFirst := &events.Event{
		Topic:          []string{TopicSymbolRedstone},
		Value:          body,
		OpArgs:         args,
		ContractID:     adapterC,
		Ledger:         52_000_000,
		TxHash:         "abcd",
		OperationIndex: 3,
		EventIndex:     0,
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	}
	closedAt, _ := time.Parse(time.RFC3339, evFirst.LedgerClosedAt)
	updatesFirst, err := decodeWritePrices(evFirst, closedAt)
	if err != nil {
		t.Fatalf("decodeWritePrices (first): %v", err)
	}

	evSecond := *evFirst
	evSecond.OperationIndex = 3 // SAME operation as evFirst
	evSecond.EventIndex = 1     // a DIFFERENT event within it
	updatesSecond, err := decodeWritePrices(&evSecond, closedAt)
	if err != nil {
		t.Fatalf("decodeWritePrices (second): %v", err)
	}

	if len(updatesFirst) != 1 || len(updatesSecond) != 1 {
		t.Fatalf("expected 1 update each, got %d and %d", len(updatesFirst), len(updatesSecond))
	}
	if updatesFirst[0].OpIndex == updatesSecond[0].OpIndex {
		t.Errorf("two events in the SAME operation (OperationIndex=3) with different EventIndex (0 vs 1) collided on OpIndex=%d — the fanout base must incorporate EventIndex, not just OperationIndex",
			updatesFirst[0].OpIndex)
	}
}

func TestDecode_FeedIDCountMismatch(t *testing.T) {
	// 2 prices, 1 feed id — simulates the freshness verifier dropping
	// one submitted feed.
	body := encodeWritePricesBody(t, relayerG,
		[]*big.Int{big.NewInt(oneBTCAt8), big.NewInt(oneETHAt8)},
		1, 2)
	args := []string{
		encodeAddressArg(t, relayerG),
		encodeStringVecArg(t, []string{"BTC"}),
		encodePayloadArg(t),
	}
	ev := &events.Event{
		Topic:  []string{TopicSymbolRedstone},
		Value:  body,
		OpArgs: args,
		TxHash: "abcd",
	}
	_, err := decodeWritePrices(ev, time.Now())
	if !errors.Is(err, ErrFeedIDCountMismatch) {
		t.Errorf("expected ErrFeedIDCountMismatch, got %v", err)
	}
}

func TestDecode_MissingOpArgs(t *testing.T) {
	body := encodeWritePricesBody(t, relayerG,
		[]*big.Int{big.NewInt(1)}, 1, 2)
	ev := &events.Event{
		Topic:  []string{TopicSymbolRedstone},
		Value:  body,
		OpArgs: nil,
		TxHash: "abcd",
	}
	_, err := decodeWritePrices(ev, time.Now())
	if !errors.Is(err, ErrMissingOpArgs) {
		t.Errorf("expected ErrMissingOpArgs, got %v", err)
	}
}

func TestDecode_UnknownFeedSkipped_KnownLands(t *testing.T) {
	// Three feeds: BTC (known), NOTAFEED (outside the ADR-0028
	// registry — e.g. a 20th feed RedStone deployed), ETH (known).
	// Middle entry must be skipped while outer two land.
	body := encodeWritePricesBody(t, relayerG,
		[]*big.Int{
			big.NewInt(oneBTCAt8),
			big.NewInt(9_000_000), // synthetic unknown-feed price
			big.NewInt(oneETHAt8),
		}, 1, 2)
	args := []string{
		encodeAddressArg(t, relayerG),
		encodeStringVecArg(t, []string{"BTC", "NOTAFEED", "ETH"}),
		encodePayloadArg(t),
	}
	ev := &events.Event{
		Topic:  []string{TopicSymbolRedstone},
		Value:  body,
		OpArgs: args,
		TxHash: "abcd",
	}
	updates, err := decodeWritePrices(ev, time.Now())
	if err != nil {
		t.Fatalf("decodeWritePrices: %v", err)
	}
	if len(updates) != 2 {
		t.Fatalf("expected 2 updates (BTC+ETH), got %d", len(updates))
	}
	btc, _ := canonical.NewCryptoAsset("BTC")
	eth, _ := canonical.NewCryptoAsset("ETH")
	if !updates[0].Asset.Equal(btc) {
		t.Errorf("updates[0].Asset = %+v want BTC", updates[0].Asset)
	}
	if !updates[1].Asset.Equal(eth) {
		t.Errorf("updates[1].Asset = %+v want ETH", updates[1].Asset)
	}
	// OpIndex preserves original-slot positions: BTC=0, ETH=2.
	if updates[0].OpIndex != 0 {
		t.Errorf("BTC OpIndex = %d, want 0", updates[0].OpIndex)
	}
	if updates[1].OpIndex != 2 {
		t.Errorf("ETH OpIndex = %d, want 2 (NOTAFEED slot 1 skipped)", updates[1].OpIndex)
	}
}

func TestDecode_AllUnknown_ErrEmptyUpdates(t *testing.T) {
	body := encodeWritePricesBody(t, relayerG,
		[]*big.Int{big.NewInt(1), big.NewInt(2)},
		1, 2)
	args := []string{
		encodeAddressArg(t, relayerG),
		// Both outside the ADR-0028 registry. Note "BENJI" alone is
		// NOT a real feed_id — the real one is
		// "BENJI_ETHEREUM_FUNDAMENTAL" (see TestDecode_RWAFeeds).
		encodeStringVecArg(t, []string{"BENJI", "NOTAFEED"}),
		encodePayloadArg(t),
	}
	ev := &events.Event{
		Topic:  []string{TopicSymbolRedstone},
		Value:  body,
		OpArgs: args,
		TxHash: "abcd",
	}
	_, err := decodeWritePrices(ev, time.Now())
	if !errors.Is(err, ErrEmptyUpdates) {
		t.Errorf("expected ErrEmptyUpdates, got %v", err)
	}
}

func TestDecode_RWAandQuoteCurrency(t *testing.T) {
	// Exercises the ADR-0028 feed registry: an RWA feed whose
	// feed_id ≠ display name, the EUR-quoted EUROC feed (the pre-#53
	// USD-hardcode bug), a plain RWA feed, and a tokenized-BTC crypto
	// feed.
	feedIDs := []string{
		"BENJI_ETHEREUM_FUNDAMENTAL", // → rwa:BENJI, USD
		"EUROC/EUR",                  // → crypto:EUROC, EUR
		"GILTS",                      // → rwa:GILTS, USD
		"SolvBTC",                    // → crypto:SolvBTC, USD
	}
	body := encodeWritePricesBody(t, relayerG,
		[]*big.Int{
			big.NewInt(1_00000000),
			big.NewInt(1_05000000),
			big.NewInt(100_00000000),
			big.NewInt(95000_00000000),
		}, 1, 2)
	args := []string{
		encodeAddressArg(t, relayerG),
		encodeStringVecArg(t, feedIDs),
		encodePayloadArg(t),
	}
	ev := &events.Event{
		Topic:  []string{TopicSymbolRedstone},
		Value:  body,
		OpArgs: args,
		TxHash: "abcd",
	}
	updates, err := decodeWritePrices(ev, time.Now())
	if err != nil {
		t.Fatalf("decodeWritePrices: %v", err)
	}
	if len(updates) != 4 {
		t.Fatalf("expected 4 updates, got %d", len(updates))
	}

	wantBenji, _ := canonical.NewRWAAsset("BENJI")
	if !updates[0].Asset.Equal(wantBenji) {
		t.Errorf("feed_id BENJI_ETHEREUM_FUNDAMENTAL → %s, want rwa:BENJI", updates[0].Asset)
	}
	if updates[0].Quote.String() != "fiat:USD" {
		t.Errorf("BENJI quote = %s, want fiat:USD", updates[0].Quote)
	}

	wantEUROC, _ := canonical.NewCryptoAsset("EUROC")
	if !updates[1].Asset.Equal(wantEUROC) {
		t.Errorf("feed_id EUROC/EUR → %s, want crypto:EUROC", updates[1].Asset)
	}
	if updates[1].Quote.String() != "fiat:EUR" {
		t.Errorf("EUROC quote = %s, want fiat:EUR (pre-#53 this was mislabelled USD)", updates[1].Quote)
	}

	wantGILTS, _ := canonical.NewRWAAsset("GILTS")
	if !updates[2].Asset.Equal(wantGILTS) {
		t.Errorf("feed_id GILTS → %s, want rwa:GILTS", updates[2].Asset)
	}

	wantSolv, _ := canonical.NewCryptoAsset("SolvBTC")
	if !updates[3].Asset.Equal(wantSolv) {
		t.Errorf("feed_id SolvBTC → %s, want crypto:SolvBTC", updates[3].Asset)
	}
}

func TestFeedRegistry_Has30Feeds(t *testing.T) {
	// The registry must cover exactly the 30 known mainnet feeds:
	// 19 captured 2026-05-22 (ADR-0028) + 11 from the 2026-07-24
	// relayer expansion. A drift here means a feed was added/removed
	// without updating the docs + this registry in lock-step.
	if len(feedRegistry) != 30 {
		t.Errorf("feedRegistry has %d feeds, want 30 (ADR-0028 + 2026-07-24 expansion)", len(feedRegistry))
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

func TestFeedRegistry_UniquePairs(t *testing.T) {
	// No two feed_ids may map to the same (Base, Quote) pair: feeds
	// arrive together in one write_prices batch, so a shared pair
	// would interleave two different quantities into one price
	// series. The 2026-07-24 expansion makes this live — e.g.
	// `SolvBTC_FUNDAMENTAL` (NAV ratio vs BTC, ~1.003) vs
	// `SolvBTC_FUNDAMENTAL/USD` (NAV in USD, ~65,430) MUST land on
	// distinct base codes, and bare `EUROC` (USD-quoted) vs
	// `EUROC/EUR` (EUR-quoted) may share a base only because the
	// quote differs.
	seen := make(map[string]string, len(feedRegistry))
	for feedID, entry := range feedRegistry {
		pair := entry.Base.String() + "|" + entry.Quote.String()
		if prev, dup := seen[pair]; dup {
			t.Errorf("feeds %q and %q both map to pair %s — same-batch double-write into one series", prev, feedID, pair)
		}
		seen[pair] = feedID
	}
}

// TestDecode_2026_07_24_ExpansionFeeds is the regression test for the
// 2026-07-24 relayer expansion (ledger 63624934): RedStone began
// publishing 11 feed_ids outside the original 19-feed registry.
// Batches mixing old + new feeds dropped the new entries per-feed;
// batches of ONLY new feeds failed whole with ErrEmptyUpdates —
// "undecodable-but-matched" projection blindness (~5,600 events).
// This all-new-feeds batch must now decode fully, with the asset /
// quote mapping verified live 2026-07-27 (see feeds.go comments).
func TestDecode_2026_07_24_ExpansionFeeds(t *testing.T) {
	feedIDs := []string{
		"EUROC", // bare — USD-quoted, unlike registered EUROC/EUR
		"USDe",
		"sUSDe",
		"savUSD_FUNDAMENTAL",
		"SolvBTC_FUNDAMENTAL/USD",
		"SolvBTC.BBN_FUNDAMENTAL/USD",
		"USDY_FUNDAMENTAL/USD",
		"USST_FUNDAMENTAL",
		"XAUm_FUNDAMENTAL/USD",
		"deJAAA_FUNDAMENTAL/USD",
		"deJTRSY_FUNDAMENTAL/USD",
	}
	// Live values captured 2026-07-27 from api.redstone.finance,
	// scaled to RedStone's fixed 8 decimals.
	prices := []*big.Int{
		big.NewInt(1_13979753),     // EUROC in USD ≈ EUR/USD
		big.NewInt(99984603),       // USDe ~0.9998
		big.NewInt(1_24071513),     // sUSDe ~1.2407
		big.NewInt(1_18773631),     // savUSD ~1.1877
		big.NewInt(6543063_913439), // SolvBTC NAV in USD ~65,430
		big.NewInt(6543063_913439), // SolvBTC.BBN NAV in USD
		big.NewInt(1_14081251),     // USDY ~1.1408
		big.NewInt(1_00957429),     // USST ~1.0096
		big.NewInt(4115_66800000),  // XAUm ~4,115.67/oz
		big.NewInt(1_04038535),     // deJAAA ~1.0404
		big.NewInt(1_03152715),     // deJTRSY ~1.0315
	}
	body := encodeWritePricesBody(t, relayerG, prices,
		1_753_500_000_000, 1_753_500_060_000)
	args := []string{
		encodeAddressArg(t, relayerG),
		encodeStringVecArg(t, feedIDs),
		encodePayloadArg(t),
	}
	ev := &events.Event{
		Topic:      []string{TopicSymbolRedstone},
		Value:      body,
		OpArgs:     args,
		ContractID: adapterC,
		Ledger:     63_624_934,
		TxHash:     "expansion",
	}
	updates, err := decodeWritePrices(ev, time.Now())
	if err != nil {
		t.Fatalf("decodeWritePrices: %v (pre-fix this was ErrEmptyUpdates — every feed unknown)", err)
	}
	if len(updates) != len(feedIDs) {
		t.Fatalf("expected %d updates, got %d — an expansion feed is still outside the registry", len(feedIDs), len(updates))
	}

	wantAssets := []struct{ asset, quote string }{
		{"crypto:EUROC", "fiat:USD"}, // NOT fiat:EUR — that's the EUROC/EUR feed
		{"crypto:USDe", "fiat:USD"},
		{"crypto:sUSDe", "fiat:USD"},
		{"crypto:savUSD_FUNDAMENTAL", "fiat:USD"},
		{"crypto:SolvBTC_FUNDAMENTAL_USD", "fiat:USD"}, // `/` normalized to `_`
		{"crypto:SolvBTC.BBN_FUNDAMENTAL_USD", "fiat:USD"},
		{"rwa:USDY", "fiat:USD"},
		{"rwa:USST", "fiat:USD"},
		{"rwa:XAUm", "fiat:USD"},
		{"rwa:deJAAA", "fiat:USD"},
		{"rwa:deJTRSY", "fiat:USD"},
	}
	for i, want := range wantAssets {
		if got := updates[i].Asset.String(); got != want.asset {
			t.Errorf("feed %q → asset %s, want %s", feedIDs[i], got, want.asset)
		}
		if got := updates[i].Quote.String(); got != want.quote {
			t.Errorf("feed %q → quote %s, want %s", feedIDs[i], got, want.quote)
		}
		// None of the expansion feeds is Invert: prices pass through
		// unchanged (the MXNe lesson — a silent inversion here would
		// be a ~1.3× to ~65,000× error depending on the feed).
		if updates[i].Price.BigInt().Cmp(prices[i]) != 0 {
			t.Errorf("feed %q price = %s, want %s unchanged", feedIDs[i], updates[i].Price, prices[i])
		}
	}

	// The /USD-suffixed SolvBTC NAV feeds must not collide with the
	// unsuffixed ratio feeds' series: same batch, different quantity
	// (~65,430 vs ~1.003).
	ratio := feedRegistry["SolvBTC_FUNDAMENTAL"]
	usd := feedRegistry["SolvBTC_FUNDAMENTAL/USD"]
	if ratio.Base.Equal(usd.Base) && ratio.Quote.Equal(usd.Quote) {
		t.Error("SolvBTC_FUNDAMENTAL and SolvBTC_FUNDAMENTAL/USD share a (base, quote) pair — NAV-ratio and NAV-in-USD would interleave in one series")
	}
}

func TestDecode_NonRedstoneTopic_Rejects(t *testing.T) {
	ev := &events.Event{Topic: []string{"AAAADwAAAAhTT1JPU1dBUAAAAAA="}}
	_, err := decodeWritePrices(ev, time.Now())
	if !errors.Is(err, ErrNotRedstoneEvent) {
		t.Errorf("expected ErrNotRedstoneEvent, got %v", err)
	}
}

// TestDecode_RealMainnetEvent_BytesWrappedBody exercises the
// exact on-wire body shape RedStone's adapter contract emits: an
// `ScVal::Bytes` wrapping the XDR-encoded WritePrices struct
// (the Rust impl uses `self.to_xdr(env).to_val()` — see
// redstone-public-contracts/packages/stellar-connector/.../event.rs).
// Earlier helper tests built the Map directly, which masked a bug
// where the decoder's Map-assertion ran against the outer Bytes
// wrapper and silently rejected every real event.
//
// Fixture: event pulled from mainnet ledger 62265977 (tx
// 349bd590…c7a8b) on 2026-04-24 via sorobanrpc.com getEvents.
// Contains one XLM price update at package_timestamp
// 2026-04-23T12:30:06Z. Feed id from the tx's write_prices args.
func TestDecode_RealMainnetEvent_BytesWrappedBody(t *testing.T) {
	// The actual event body from ledger 62265977. Outer wrapper is
	// ScVal::Bytes(248); inner parse yields Map{updater, updated_feeds}.
	body := "AAAADQAAAPgAAAARAAAAAQAAAAIAAAAPAAAADXVwZGF0ZWRfZmVlZHMAAAAAAAAQAAAAAQAAAAEAAAARAAAAAQAAAAMAAAAPAAAAEXBhY2thZ2VfdGltZXN0YW1wAAAAAAAABQAAAZ2/ru8wAAAADwAAAAVwcmljZQAAAAAAAAsAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABA0AaYAAAAA8AAAAPd3JpdGVfdGltZXN0YW1wAAAAAAUAAAGdv68KiAAAAA8AAAAHdXBkYXRlcgAAAAASAAAAAAAAAAAk1wP6EQQ6Z6YlFesVDZAkQ3o7tjIdDJoRh0/hHzC1Bw=="

	// OpArgs built to match the real tx's write_prices(updater,
	// feed_ids, payload) call with feed_ids=["XLM"] — matching the
	// single entry in updated_feeds.
	args := []string{
		encodeAddressArg(t, relayerG),
		encodeStringVecArg(t, []string{"XLM"}),
		encodePayloadArg(t),
	}

	ev := &events.Event{
		Topic:          []string{TopicSymbolRedstone},
		Value:          body,
		OpArgs:         args,
		ContractID:     adapterC,
		Ledger:         62_265_977,
		TxHash:         "349bd590c679a9d69ac0ff3eb49a673f95cf9d77016fc3d019eb654c772c7a8b",
		OperationIndex: 0,
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

	xlm, _ := canonical.NewCryptoAsset("XLM")
	if !updates[0].Asset.Equal(xlm) {
		t.Errorf("Asset = %+v want XLM", updates[0].Asset)
	}
	// Price from the event body: 4349500000 (0x103401A60 = U256).
	if updates[0].Price.BigInt().Cmp(big.NewInt(4_349_500_000)) != 0 {
		t.Errorf("Price = %s want 4349500000", updates[0].Price)
	}
}

// mxneUSDMXNAt8 is RedStone's on-chain MXNe value in market-FX
// orientation: ~17.3911 pesos per USD (USDMXN), scaled to 8 decimals.
const mxneUSDMXNAt8 = int64(1_739_110_000) // 17.3911 × 1e8

// TestDecode_MXNe_InvertedToTokenInUSD is the FIX-2 regression.
// RedStone publishes MXNe as USDMXN (~17.39 pesos/USD); our registry
// marks it Invert, so the decoder must reciprocate to MXNe-in-USD
// (~0.0575) — matching every other feed (quote fiat:USD) and
// reflector-fx MXN (~0.0573, verified live 2026-07-07). Before the
// fix this served ~17.39, implying 1 MXNe = $17.39 — a ~302× error.
func TestDecode_MXNe_InvertedToTokenInUSD(t *testing.T) {
	body := encodeWritePricesBody(t, relayerG,
		[]*big.Int{big.NewInt(mxneUSDMXNAt8)}, 1_745_000_000_000, 1_745_000_060_000)
	args := []string{
		encodeAddressArg(t, relayerG),
		encodeStringVecArg(t, []string{"MXNe"}),
		encodePayloadArg(t),
	}
	ev := &events.Event{
		Topic:          []string{TopicSymbolRedstone},
		Value:          body,
		OpArgs:         args,
		ContractID:     adapterC,
		Ledger:         52_000_001,
		TxHash:         "mxne",
		OperationIndex: 0,
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	}
	closedAt, _ := time.Parse(time.RFC3339, ev.LedgerClosedAt)

	updates, err := decodeWritePrices(ev, closedAt)
	if err != nil {
		t.Fatalf("decodeWritePrices: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}

	mxne, _ := canonical.NewCryptoAsset("MXNe")
	if !updates[0].Asset.Equal(mxne) {
		t.Errorf("asset = %s want crypto:MXNe", updates[0].Asset)
	}
	if updates[0].Quote.String() != "fiat:USD" {
		t.Errorf("quote = %s want fiat:USD", updates[0].Quote)
	}
	// Value must be reciprocated: ~0.0575 in USD, NOT the raw ~17.39.
	scaled, _ := new(big.Rat).SetFrac(updates[0].Price.BigInt(), big.NewInt(100_000_000)).Float64()
	if scaled < 0.055 || scaled > 0.060 {
		t.Errorf("MXNe-in-USD = %v, want ~0.0575 (raw USDMXN ~17.39 means the inversion did not run)", scaled)
	}
	// Guard explicitly against the pre-fix (un-inverted) value.
	if updates[0].Price.BigInt().Cmp(big.NewInt(mxneUSDMXNAt8)) == 0 {
		t.Error("MXNe price stored un-inverted (still ~17.39 pesos/USD) — the Invert path did not fire")
	}
}

// TestDecode_NonInvertedCurrencyFeed_Unchanged pins that inversion is
// scoped to Invert feeds only: the sibling Mexican-peso RWA feed
// CETES (published dollars-per-unit, ~$0.067) passes through
// unchanged. A regression that inverted every currency feed would
// turn CETES into ~14.85 and be caught here.
func TestDecode_NonInvertedCurrencyFeed_Unchanged(t *testing.T) {
	const cetesUSDAt8 = int64(6_734_600) // 0.067346 × 1e8
	body := encodeWritePricesBody(t, relayerG,
		[]*big.Int{big.NewInt(cetesUSDAt8)}, 1_745_000_000_000, 1_745_000_060_000)
	args := []string{
		encodeAddressArg(t, relayerG),
		encodeStringVecArg(t, []string{"CETES"}),
		encodePayloadArg(t),
	}
	ev := &events.Event{
		Topic:      []string{TopicSymbolRedstone},
		Value:      body,
		OpArgs:     args,
		ContractID: adapterC,
		Ledger:     52_000_002,
		TxHash:     "cetes",
	}
	updates, err := decodeWritePrices(ev, time.Now())
	if err != nil {
		t.Fatalf("decodeWritePrices: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	if updates[0].Price.BigInt().Cmp(big.NewInt(cetesUSDAt8)) != 0 {
		t.Errorf("CETES price = %s, want %d unchanged (non-Invert feed must not be reciprocated)",
			updates[0].Price, cetesUSDAt8)
	}
	if updates[0].Quote.String() != "fiat:USD" {
		t.Errorf("CETES quote = %s want fiat:USD", updates[0].Quote)
	}
}

// TestReciprocalAtScale checks the exact big.Int reciprocal used by
// the Invert path (ADR-0003 — no float/int64 truncation).
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

// TestDecode_EmptyOnWireBatch_IsRecognizedNoOp pins the empty-batch
// no-op semantics against REAL lake bytes (ledger 63,699,567, contract
// CA526Y2N…, captured 2026-07-29): the adapter emits
// `{updated_feeds: [], updater}` (Bytes-wrapped) when its freshness
// verifier drops every candidate feed. ~1.5% of ALL REDSTONE events
// ever have this shape; treating it as an error kept redstone
// projection_ok=false under honest-blind completeness accounting.
// It must decode to ZERO updates with NO error.
func TestDecode_EmptyOnWireBatch_IsRecognizedNoOp(t *testing.T) {
	// Real event body from the lake (base64 data_xdr, verbatim).
	const realBody = "AAAADQAAAGwAAAARAAAAAQAAAAIAAAAPAAAADXVwZGF0ZWRfZmVlZHMAAAAAAAAQAAAAAQAAAAAAAAAPAAAAB3VwZGF0ZXIAAAAAEgAAAAAAAAAAI55i1HMFV5Z4R37dzgSnns7qJjbxzw6uCHkzbmb6XRI="

	args := []string{
		encodeAddressArg(t, relayerG),
		encodeStringVecArg(t, []string{}), // empty feed_ids matches empty batch
		encodePayloadArg(t),
	}
	ev := &events.Event{
		Topic:          []string{TopicSymbolRedstone},
		Value:          realBody,
		OpArgs:         args,
		ContractID:     adapterC,
		Ledger:         63_699_567,
		TxHash:         "efgh",
		OperationIndex: 0,
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
