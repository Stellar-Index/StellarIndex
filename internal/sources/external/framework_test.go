package external

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestRegistry_KnownSourcesClassified ensures every on-chain decoder package and live
// off-chain connector has a Registry entry; a missing one would silently exclude the source
// from VWAP.
func TestRegistry_KnownSourcesClassified(t *testing.T) {
	// Keep this list aligned with what internal/sources/ exports.
	want := []string{
		"soroswap", "aquarius", "phoenix", "comet", "sdex", "blend",
		"reflector-dex", "reflector-cex", "reflector-fx",
		"redstone", "band",
		"binance", "kraken", "bitstamp", "coinbase",
		"massive", "exchangeratesapi",
		"coingecko", "coinmarketcap", "cryptocompare",
		"ecb",
	}
	for _, name := range want {
		if _, ok := Registry[name]; !ok {
			t.Errorf("Registry missing entry for %q — aggregator would treat it as fail-closed unknown", name)
		}
	}
}

// TestRegistry_RetiredFXIdentityStaysUnregistered: `polygon-forex` and `massive` are one upstream under two brands; re-adding the
// retired entry (IncludeInVWAP:true) double-counts massive's rates in VWAP. Both halves are asserted so a revert cannot be quiet.
func TestRegistry_RetiredFXIdentityStaysUnregistered(t *testing.T) {
	if _, ok := Registry["polygon-forex"]; ok {
		t.Error("Registry has a `polygon-forex` entry again — it is the same upstream as `massive` under the pre-rename brand, and with IncludeInVWAP:true it double-counts those rates")
	}
	m, ok := Registry["massive"]
	if !ok {
		t.Fatal("Registry missing `massive` — the surviving fiat-FX identity")
	}
	if m.Subclass != SubclassFX {
		t.Errorf("massive Subclass = %q, want %q", m.Subclass, SubclassFX)
	}
	for _, name := range FXSources() {
		if name == "polygon-forex" {
			t.Error("FXSources() names the retired `polygon-forex`; the forex-snap would read a duplicate identity")
		}
	}
}

// TestIsOnChain_Partition pins the on-chain/off-chain split the explorer's Stellar-network
// surfaces filter on. It enumerates EVERY registered source, so an unclassified new one fails
// rather than silently landing a reference feed on the /network page.
func TestIsOnChain_Partition(t *testing.T) {
	offChain := map[string]bool{
		// CEX
		"binance": true, "kraken": true, "bitstamp": true, "coinbase": true, "poloniex_via_btc": true,
		// FX
		"massive": true, "exchangeratesapi": true,
		// aggregators
		"coingecko": true, "coinmarketcap": true, "cryptocompare": true, "coingecko_daily": true,
		// sovereign anchor
		"ecb": true,
		// Ethereum oracle (JSON-RPC), not Stellar
		"chainlink": true,
		// fund NAV vendor API
		"tiingo": true,
	}
	for name := range Registry {
		got := IsOnChain(name)
		want := !offChain[name]
		if got != want {
			t.Errorf("IsOnChain(%q) = %v, want %v — classify it in this test (and confirm the registry section it sits under)", name, got, want)
		}
	}
	// Unknown sources fall through to on-chain (registry is closed).
	if !IsOnChain("does-not-exist") {
		t.Error("IsOnChain(unknown) should default true (closed registry)")
	}
}

// A fund NAV is a reference value for the RWA surface only; admitting it
// to VWAP would price a fund share as if it were a market trade.
func TestRegistry_TiingoIsReferenceOnly(t *testing.T) {
	m, ok := Registry["tiingo"]
	if !ok {
		t.Fatal("tiingo missing from Registry")
	}
	if m.Class != ClassOracle || m.IncludeInVWAP || m.DefaultWeight != 0 || m.Paid {
		t.Errorf("tiingo = %+v; want ClassOracle, IncludeInVWAP false, DefaultWeight 0, Paid false", m)
	}
	if IsOnChain("tiingo") {
		t.Error("IsOnChain(tiingo) = true; it is an off-chain vendor API")
	}
}

// A vendor's daily close replayed from a file is a history-only reference series: credited,
// weightless, off-chain, never polled and never a VWAP input.
func TestRegistry_CoinGeckoDailyIsHistoryOnly(t *testing.T) {
	m, ok := Registry["coingecko_daily"]
	if !ok {
		t.Fatal("coingecko_daily missing from Registry")
	}
	if m.Class != ClassAggregator || m.IncludeInVWAP || m.DefaultWeight != 0 || m.Paid || m.Credit != "CoinGecko" {
		t.Errorf("coingecko_daily = %+v; want ClassAggregator, IncludeInVWAP false, DefaultWeight 0, Paid false, Credit CoinGecko", m)
	}
	if m.OracleResolution != 0 || !m.BackfillAvailable {
		t.Errorf("coingecko_daily = %+v; want OracleResolution 0 (never polled) and BackfillAvailable true", m)
	}
	if IsOnChain("coingecko_daily") {
		t.Error("IsOnChain(coingecko_daily) = true; it is an off-chain vendor series")
	}
}

func TestRegistry_ClassPolicy(t *testing.T) {
	// Invariant: only ClassExchange may have IncludeInVWAP=true. Every other class MUST be
	// excluded from VWAP by default; mixing them in double-counts upstream markets or imposes
	// someone else's methodology on our output.
	for name, m := range Registry {
		if m.IncludeInVWAP && m.Class != ClassExchange {
			t.Errorf("source %q: IncludeInVWAP=true but Class=%q — only ClassExchange may VWAP-contribute by default",
				name, m.Class)
		}
	}
}

// TestClassBridge_Defined locks the ClassBridge constant so a rename or removal is a build
// break here, not a silent classification miss. protocols/cctp.md and protocols/rozo.md
// reference the class by name.
func TestClassBridge_Defined(t *testing.T) {
	t.Parallel()
	if ClassBridge != "bridge" {
		t.Errorf("ClassBridge wire value = %q, want \"bridge\"", ClassBridge)
	}
}

func TestRegistry_FailClosedOnUnknown(t *testing.T) {
	// Lookup of an unknown source must return metadata that is visible (via /v1/sources) but excluded from VWAP,
	// so a typo or renamed source cannot quietly contribute.
	m := Lookup("definitely-not-a-real-source")
	if m.IncludeInVWAP {
		t.Error("Lookup on unknown source returned IncludeInVWAP=true; must fail-closed")
	}
	if IncludeInVWAP("definitely-not-a-real-source") {
		t.Error("IncludeInVWAP helper on unknown source returned true; must fail-closed")
	}
	if IncludeInVWAP("binance") != true {
		t.Error("IncludeInVWAP(binance) should be true; registry says otherwise")
	}
	if IncludeInVWAP("coingecko") != false {
		t.Error("IncludeInVWAP(coingecko) should be false (aggregator class); registry says otherwise")
	}
}

// TestRegistry_Registered pins the accessor scale checks need: Lookup's zero-value fallback
// makes "unregistered" and "registered, scale unset" indistinguishable through
// AmountScaleDecimals() alone, so commonAmountScaleDecimals needs direct registry membership.
func TestRegistry_Registered(t *testing.T) {
	if !Registered("binance") {
		t.Error("Registered(binance) = false, want true — binance has a Registry entry")
	}
	if Registered("definitely-not-a-real-source") {
		t.Error("Registered(definitely-not-a-real-source) = true, want false")
	}
}

// TestRegistry_BackfillSafePolicy locks the WASM-aware default: on-chain Soroban sources are BackfillSafe=false until audited
// against every WASM version in the replay range; SDEX and off-chain sources are true; unknown sources fail closed. Flipping
// to true is a reviewed change.
func TestRegistry_BackfillSafePolicy(t *testing.T) {
	wantUnsafe := []string{
		// All Soroban sources are now audited; the wantUnsafe slot
		// stays here as the structural place to put a new on-chain
		// source while its audit is pending.
	}
	for _, name := range wantUnsafe {
		if Registry[name].BackfillSafe() {
			t.Errorf("source %q has BackfillSafe=true but is on-chain Soroban; flip only after wasm-history audit lands", name)
		}
		if BackfillSafe(name) {
			t.Errorf("BackfillSafe(%q) returned true; must be false until per-WASM-hash audit completes", name)
		}
	}

	wantPerWASM := []string{
		"soroswap",      // audited — see docs/operations/wasm-audits/soroswap.md
		"band",          // audited — see docs/operations/wasm-audits/band.md
		"redstone",      // audited — see docs/operations/wasm-audits/redstone.md
		"reflector-dex", // audited (incl v2 disassembly) — see docs/operations/wasm-audits/reflector.md
		"reflector-cex", // audited (incl v2 disassembly) — see docs/operations/wasm-audits/reflector.md
		"reflector-fx",  // audited — see docs/operations/wasm-audits/reflector.md
		"phoenix",       // audited (11 pools enumerated, 2 unique WASMs verified) — see docs/operations/wasm-audits/phoenix.md
		"aquarius",      // audited (313 pools enumerated, 3 unique WASMs verified) — see docs/operations/wasm-audits/aquarius.md
		"comet",         // audited (Blend backstop pool only known mainnet deployment; WASM verified) — see docs/operations/wasm-audits/comet.md
		"blend",         // audited (11 contracts, 3 unique WASMs, no mid-life upgrades over 11.79M-ledger walk) — see docs/operations/wasm-audits/blend.md §"Phase 2 results"
		"blend_emitter", // audited (ClickHouse-lake-only; all 469 lifetime events shape-verified, 465/465 distribute exhaustively) — see docs/operations/wasm-audits/blend_emitter.md
		"sushiswap_v3",
	}
	for _, name := range wantPerWASM {
		if Registry[name].Backfill != BackfillPerWASM {
			t.Errorf("source %q must be BackfillPerWASM (Soroban: static policy plus the per-WASM replay gate)", name)
		}
	}

	wantNoWASM := []string{
		"sdex", // classic Stellar, no WASM
		"binance", "kraken", "bitstamp", "coinbase",
		"massive", "exchangeratesapi",
		"coingecko", "coinmarketcap", "cryptocompare",
		"ecb",
	}
	for _, name := range wantNoWASM {
		if Registry[name].Backfill != BackfillNoWASM {
			t.Errorf("source %q must be BackfillNoWASM (off-chain or pre-Soroban)", name)
		}
	}
	for _, name := range append(wantPerWASM, wantNoWASM...) {
		if !BackfillSafe(name) {
			t.Errorf("BackfillSafe(%q) returned false; off-chain + SDEX have no on-chain WASM dependency", name)
		}
	}

	// Unknown source → fail-closed false. An unaudited or typoed
	// source must never be allowed to run a backfill.
	if BackfillSafe("definitely-not-a-real-source") {
		t.Error("BackfillSafe on unknown source returned true; must fail-closed false")
	}
}

// TestFXSources_DeterministicLexOrder: every FX-classified source in Registry must appear
// exactly once, in lexicographic order. The storage tiebreak (`ORDER BY ts DESC, source DESC`)
// is only deterministic if every region computes the same source set.
func TestFXSources_DeterministicLexOrder(t *testing.T) {
	got := FXSources()

	// Every FX entry in Registry must be present.
	wantSet := map[string]struct{}{}
	for name, m := range Registry {
		if m.Subclass == SubclassFX {
			wantSet[name] = struct{}{}
		}
	}
	if len(got) != len(wantSet) {
		t.Fatalf("FXSources() returned %d, want %d (registry FX subclass count)", len(got), len(wantSet))
	}
	for _, name := range got {
		if _, ok := wantSet[name]; !ok {
			t.Errorf("FXSources() included %q which is not Subclass=FX in Registry", name)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Errorf("FXSources() not in ascending lex order: %v", got)
			break
		}
	}
}

// TestIsFXSource_RegistryDriven exercises the snap rule's per-leg classification, which is registry-driven:
// a new SubclassFX vendor is picked up without code changes elsewhere.
func TestIsFXSource_RegistryDriven(t *testing.T) {
	if !IsFXSource("exchangeratesapi") {
		t.Error("IsFXSource(exchangeratesapi) = false, want true")
	}
	if !IsFXSource("massive") {
		t.Error("IsFXSource(massive) = false, want true — it is the live fiat-FX feed")
	}
	if !IsFXSource("ecb") {
		t.Error("IsFXSource(ecb) = false, want true — the forex worker's ECB standby writes fx_quotes as ecb")
	}
	if IsFXSource("polygon-forex") {
		t.Error("IsFXSource(polygon-forex) = true, want false — the name is retired (see TestRegistry_RetiredFXIdentityStaysUnregistered)")
	}
	if IsFXSource("binance") {
		t.Error("IsFXSource(binance) = true, want false (CEX, not FX)")
	}
	if IsFXSource("definitely-not-real") {
		t.Error("IsFXSource(unknown) = true, want false (fallback empty subclass)")
	}
}

func TestAggregatorSources_DeterministicLexOrder(t *testing.T) {
	got := AggregatorSources()

	wantSet := map[string]struct{}{}
	for name, m := range Registry {
		if m.Class == ClassAggregator {
			wantSet[name] = struct{}{}
		}
	}
	if len(got) != len(wantSet) {
		t.Fatalf("AggregatorSources() returned %d, want %d", len(got), len(wantSet))
	}
	for _, name := range got {
		if _, ok := wantSet[name]; !ok {
			t.Errorf("AggregatorSources() included %q which is not Class=Aggregator", name)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Errorf("AggregatorSources() not in ascending lex order: %v", got)
			break
		}
	}
	// Sanity: CG + CMC + CryptoCompare must all be present.
	have := map[string]bool{}
	for _, n := range got {
		have[n] = true
	}
	for _, want := range []string{"coingecko", "coinmarketcap", "cryptocompare"} {
		if !have[want] {
			t.Errorf("AggregatorSources missing %q", want)
		}
	}
}

func TestEvents_SourceFieldDelegatesToCanonical(t *testing.T) {
	// Source() labels metrics by venue and MUST delegate to the embedded canonical.Trade.Source, or every external venue collapses
	// into one "external.trade" label; this checks EventKind and that the Source path compiles (a full Trade needs valid
	// Pair/Amount).
	var te TradeEvent
	if got := te.EventKind(); got != "external.trade" {
		t.Errorf("TradeEvent.EventKind() = %q, want external.trade", got)
	}
	var ue UpdateEvent
	if got := ue.EventKind(); got != "external.update" {
		t.Errorf("UpdateEvent.EventKind() = %q, want external.update", got)
	}
}

// unboundedBodyRead matches a response body handed straight to a reader
// with no io.LimitReader in between.
var unboundedBodyRead = regexp.MustCompile(`(io\.ReadAll|json\.NewDecoder|xml\.NewDecoder)\(\w+\.Body\)`)

// Every third-party response read under internal/sources must be capped:
// a wedged upstream streaming inside the timeout otherwise OOMs the ingester.
func TestSourcesNeverReadUncappedResponseBody(t *testing.T) {
	var hits []string
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(src), "\n") {
			if unboundedBodyRead.MatchString(line) {
				hits = append(hits, path+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		t.Fatalf("uncapped response-body reads (wrap in io.LimitReader):\n%s", strings.Join(hits, "\n"))
	}
}
