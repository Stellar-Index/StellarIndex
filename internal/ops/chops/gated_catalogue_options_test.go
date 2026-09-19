package chops

import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	blend_emitter "github.com/Stellar-Index/StellarIndex/internal/sources/blend_emitter"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/defindex"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	sushiswap_v3 "github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
	"github.com/Stellar-Index/StellarIndex/internal/sources/upshift"
)

// registryOnlyContract stands for a pool / vault an operator admitted by
// writing a protocol_contracts row — the documented seam for adding a
// contract without a redeploy. It is in NO decoder's in-code curated set
// and has no factory creation event, so the only way any decoder can know
// it is the warmed registry options. Shape-valid filler, deliberately not a
// real contract.
const registryOnlyContract = "CREGISTRYONLYAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// gatedProbeTopics gives, per gated source, the topics of one business
// event that source's Matches() gates on CHILD identity (reg.Has). Every
// row is calibrated before it is trusted — see calibrateProbe — so a
// constant that stops classifying fails the test instead of hollowing it.
var gatedProbeTopics = map[string][]string{
	comet.SourceName:         {comet.TopicSymbolPool, comet.TopicSymbolSwap},
	blend_emitter.SourceName: {blend_emitter.TopicSymbolDistribute},
	phoenix.SourceName:       {phoenix.TopicSymbolSwap, phoenix.TopicSymbolSender},
	blend.SourceName:         {blend.TopicSymbolSupply},
	aquarius.SourceName:      {aquarius.TopicSymbolTrade},
	sushiswap_v3.SourceName:  {sushiswap_v3.TopicSymbolSwap},
	upshift.SourceName:       {upshift.TopicSymbolDeposit},
	defindex.SourceName:      {defindex.TopicPrefixStrategy, defindex.TopicSymbolDeposit},
}

func probeEvent(source string) events.Event {
	return events.Event{
		Type:                     "contract",
		Ledger:                   64_000_000,
		LedgerClosedAt:           "2026-09-01T00:00:00Z",
		ContractID:               registryOnlyContract,
		TxHash:                   strings.Repeat("ab", 32),
		InSuccessfulContractCall: true,
		Topic:                    gatedProbeTopics[source],
	}
}

// calibrateProbe proves the probe measures the gate and nothing else: the
// source's own constructor admits it when — and only when — the contract is
// seeded. Without this a probe whose topic no longer classifies would make
// every Matches() below false for the wrong reason.
func calibrateProbe(t *testing.T, source string) {
	t.Helper()
	meta, ok := pipeline.GatedMetaFor(source)
	if !ok {
		t.Fatalf("%s is not a gated source", source)
	}
	if _, ok := gatedProbeTopics[source]; !ok {
		t.Fatalf("gated source %q has no probe topics — add a row to gatedProbeTopics; "+
			"without one this test cannot see whether its catalogue decoder is warmed", source)
	}
	ev := probeEvent(source)
	if meta.NewDecoder().Matches(ev) {
		t.Fatalf("%s: the BARE decoder already admits %s — the probe is not identity-gated", source, registryOnlyContract)
	}
	if !meta.NewDecoder(contractid.WithSeed([]string{registryOnlyContract})).Matches(ev) {
		t.Fatalf("%s: a decoder seeded with %s still rejects the probe — its topics no longer classify", source, registryOnlyContract)
	}
}

// warmedLike builds the options map the way pipeline.GatedRegistryOptions
// does for a protocol_contracts table holding exactly one extra row per
// gated source: a WithSeed of the loaded ids.
func warmedLike() map[string][]contractid.Option {
	out := map[string][]contractid.Option{}
	for _, name := range pipeline.GatedSourceNames() {
		out[name] = []contractid.Option{contractid.WithSeed([]string{registryOnlyContract})}
	}
	return out
}

func bareCatalogue(t *testing.T) []reconSource {
	t.Helper()
	cat, _, err := buildReconciliationCatalogue(config.Config{})
	if err != nil {
		t.Fatalf("buildReconciliationCatalogue: %v", err)
	}
	return cat
}

// TestApplyGatedOptions_EveryGatedSourceAdmitsARegistryOnlyContract pins
// RLT-430. The re-derive catalogue built every gated decoder bare, so a
// contract admitted only through protocol_contracts was decoded by the live
// indexer and rejected by every re-derive: phantom served rows on the
// completeness axis, and a truncate + `ch-rebuild -write` that rebuilt the
// table without them.
//
// Lockstep over pipeline.GatedSourceNames(), not a hand-kept list: a ninth
// gated source has to appear in the catalogue AND be warmed, or this fails.
func TestApplyGatedOptions_EveryGatedSourceAdmitsARegistryOnlyContract(t *testing.T) {
	bare := bareCatalogue(t)
	warmed, err := applyGatedOptions(bare, warmedLike())
	if err != nil {
		t.Fatalf("applyGatedOptions: %v", err)
	}

	names := pipeline.GatedSourceNames()
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("no gated sources — this test is asserting nothing")
	}
	for _, name := range names {
		calibrateProbe(t, name)
		ev := probeEvent(name)

		src := catalogueSource(t, warmed, name)
		if src.dec == nil {
			t.Errorf("%s: warmed catalogue entry has no decoder", name)
			continue
		}
		if !src.dec.Matches(ev) {
			t.Errorf("%s: the re-derive decoder rejects a contract that protocol_contracts admits — "+
				"the live indexer decodes it, so its served rows read as phantoms and a "+
				"truncate + ch-rebuild -write deletes them (RLT-430)", name)
		}

		// The throwaway that enumerates the -ch lake prefilter must carry
		// the same gate, or the contract is filtered out of the read before
		// the (correctly warmed) decoder ever sees it.
		if src.newGatedDec != nil {
			if !containsStr(src.newGatedDec().GatedContractSet(), registryOnlyContract) {
				t.Errorf("%s: newGatedDec omits the registry-only contract — gatedPrefilter would scope it out of the lake read", name)
			}
		}
		// Same for a static contractIDs list: ch-rebuild / ch-reproject
		// apply it as a hard per-event filter ahead of Matches().
		if len(src.contractIDs) > 0 && !containsStr(src.contractIDs, registryOnlyContract) {
			t.Errorf("%s: static contractIDs filter omits the registry-only contract — "+
				"ch-rebuild drops its events before the decoder sees them", name)
		}

		// The input catalogue must be left alone (callers may hold it).
		if catalogueSource(t, bare, name).dec.Matches(ev) {
			t.Errorf("%s: applyGatedOptions mutated the catalogue it was given", name)
		}
	}
}

// TestApplyGatedOptions_EmptyRegistryIsTheBareCatalogue — with nothing in
// protocol_contracts the warmed catalogue must be indistinguishable from the
// bare one: same curated gate, and a static contractIDs filter that is
// byte-identical (same members, same ORDER) to the in-code list.
func TestApplyGatedOptions_EmptyRegistryIsTheBareCatalogue(t *testing.T) {
	bare := bareCatalogue(t)
	empty := map[string][]contractid.Option{}
	for _, name := range pipeline.GatedSourceNames() {
		empty[name] = []contractid.Option{contractid.WithSeed(nil)}
	}
	warmed, err := applyGatedOptions(bare, empty)
	if err != nil {
		t.Fatalf("applyGatedOptions: %v", err)
	}
	if len(warmed) != len(bare) {
		t.Fatalf("catalogue length changed: %d → %d", len(bare), len(warmed))
	}
	for i := range bare {
		if warmed[i].name != bare[i].name {
			t.Fatalf("catalogue order changed at %d: %s → %s", i, bare[i].name, warmed[i].name)
		}
		if strings.Join(warmed[i].contractIDs, ",") != strings.Join(bare[i].contractIDs, ",") {
			t.Errorf("%s: contractIDs changed with an empty registry:\n bare   %v\n warmed %v",
				bare[i].name, bare[i].contractIDs, warmed[i].contractIDs)
		}
		if _, gated := pipeline.GatedMetaFor(bare[i].name); !gated {
			continue
		}
		if warmed[i].dec.Matches(probeEvent(bare[i].name)) {
			t.Errorf("%s: admits a contract nobody registered", bare[i].name)
		}
	}
	// A curated member must survive the rebuild (the in-code seed is part of
	// every constructor, not something the options replace).
	ph := catalogueSource(t, warmed, phoenix.SourceName)
	ev := probeEvent(phoenix.SourceName)
	ev.ContractID = phoenix.MainnetPools[0]
	if !ph.dec.Matches(ev) {
		t.Error("phoenix: the curated pool set was lost when the decoder was rebuilt with options")
	}
}

// TestApplyGatedOptions_FailsClosedOnAMissingSource — GatedRegistryOptions
// returns an entry for EVERY gated source, so a missing key is a wiring
// bug. Keeping the bare decoder for it would silently restore RLT-430.
func TestApplyGatedOptions_FailsClosedOnAMissingSource(t *testing.T) {
	opts := warmedLike()
	delete(opts, comet.SourceName)
	if _, err := applyGatedOptions(bareCatalogue(t), opts); err == nil {
		t.Fatal("applyGatedOptions accepted an options map with no entry for comet — it would re-derive on the bare seed")
	} else if !strings.Contains(err.Error(), comet.SourceName) {
		t.Fatalf("error does not name the source: %v", err)
	}
}

func TestUnionContractIDs(t *testing.T) {
	base := []string{"CB", "CA"}
	got := unionContractIDs(base, []string{"CZ", "CA", "CC", "CZ"})
	if want := "CB,CA,CC,CZ"; strings.Join(got, ",") != want {
		t.Errorf("unionContractIDs = %v, want %s (curated order kept, extras sorted + deduped)", got, want)
	}
	got[0] = "mutated"
	if base[0] != "CB" {
		t.Error("unionContractIDs aliases its base slice")
	}
}

// TestCHRebuild_WarmsTheCatalogueGatesBeforeAnythingReadsThem pins the
// ch-rebuild call site. chRebuild needs ClickHouse and Postgres past the
// config load, so — like the BackfillSafe legs — the wiring is pinned at the
// source: the warm exists, it runs on the freshly built catalogue, and it
// precedes both the preseed (which seeds INTO the decoders the warm
// rebuilds) and the writer.
func TestCHRebuild_WarmsTheCatalogueGatesBeforeAnythingReadsThem(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("ch_rebuild.go")
	if err != nil {
		t.Fatalf("read ch_rebuild.go: %v", err)
	}
	body := funcBodyFrom(string(b), "chRebuild")
	if body == "" {
		t.Fatal("chRebuild not found — this test is asserting nothing")
	}
	catalogue := strings.Index(body, "buildReconciliationCatalogue(cfg)")
	preseed := strings.Index(body, "preseedFactoryChildren(")
	writer := strings.Index(body, "drainAndWrite(")
	if catalogue < 0 || preseed < 0 || writer < 0 {
		t.Fatalf("anchors not found (catalogue %d, preseed %d, writer %d) — this test is asserting nothing",
			catalogue, preseed, writer)
	}
	warm := strings.Index(body, "warmCatalogueGates(ctx, store, logger, cat)")
	switch {
	case warm < 0:
		t.Error("chRebuild never warms the catalogue's gated decoders from protocol_contracts: " +
			"`ch-rebuild -write` re-derives on the bare in-code seed and rebuilds a table without " +
			"the contracts an operator admitted (RLT-430)")
	case warm < catalogue:
		t.Error("the gate warm runs BEFORE the catalogue is built")
	case warm > preseed:
		t.Error("the gate warm runs AFTER preseedFactoryChildren — it rebuilds the decoders and discards what the preseed registered")
	case warm > writer:
		t.Error("the gate warm runs AFTER the writer")
	}
}
