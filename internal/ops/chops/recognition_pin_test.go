package chops

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	blend_backstop "github.com/Stellar-Index/StellarIndex/internal/sources/blend_backstop"
	"github.com/Stellar-Index/StellarIndex/internal/sources/rozo"
)

// staticOwners rebuilds the contract → source map the way
// computeCompleteness seeds it BEFORE the registry fold: every catalogue
// source's static contractIDs. rozo and blend_backstop are not
// gated-registry sources, so loadRegistryOwners (protocol_contracts +
// soroswap_pairs) can never name their contracts; this static pass is the
// only path that can put them in ownerOf.
func staticOwners(t *testing.T) (map[string]string, []reconSource) {
	t.Helper()
	cat, _, err := buildReconciliationCatalogue(config.Config{})
	if err != nil {
		t.Fatalf("buildReconciliationCatalogue: %v", err)
	}
	ownerOf := map[string]string{}
	for _, src := range cat {
		for _, c := range src.contractIDs {
			ownerOf[c] = src.name
		}
	}
	return ownerOf, cat
}

func catalogueSource(t *testing.T, cat []reconSource, name string) reconSource {
	t.Helper()
	for _, src := range cat {
		if src.name == name {
			return src
		}
	}
	t.Fatalf("catalogue has no %q source", name)
	return reconSource{}
}

// TestRecognitionAttribution_RozoAndBackstopOwnTheirContracts pins F071.
// Before the fix neither source declared contractIDs, so an unhandled
// topic on a Rozo payment contract or on the Blend backstop fell into the
// system-wide `unattributed` bucket and the source's own recognition_ok
// was structurally unable to go false — the 2026-07-07 rozo blind spot's
// class, with the alert that used to cover it removed by #465.
//
// Walks the real chain (catalogue → ownerOf → attributeRecognitionGaps →
// sourceRecognitionOK) for EVERY contract each decoder claims, so a
// contract added to a decoder without being pinned here fails too.
func TestRecognitionAttribution_RozoAndBackstopOwnTheirContracts(t *testing.T) {
	ownerOf, cat := staticOwners(t)

	cases := []struct {
		source    string
		contracts []string
	}{
		{"rozo", rozo.MainnetPaymentContracts},
		{blend_backstop.SourceName, []string{blend_backstop.MainnetBackstopV2, blend_backstop.MainnetBackstopV1}},
	}
	for _, tc := range cases {
		src := catalogueSource(t, cat, tc.source)
		if len(tc.contracts) == 0 {
			t.Fatalf("%s: no contracts to probe — this case is asserting nothing", tc.source)
		}
		for _, contract := range tc.contracts {
			gapLedger := src.genesis + 1_000
			gaps := []completeness.RecognitionGap{{
				ContractID: contract, Topic0Sym: "topic_no_arm_handles",
				MinLedger: gapLedger, MaxLedger: gapLedger, Count: 1,
				Reason: "no decoder matches",
			}}
			recBySource, unattributed := attributeRecognitionGaps(ownerOf, gaps)

			if len(unattributed) != 0 {
				t.Errorf("%s: an unrecognised topic on its own contract %s fell into the system-wide "+
					"bucket (unattributed=%d) — the source's recognition axis cannot see it (F071)",
					tc.source, contract, len(unattributed))
			}
			got := recBySource[tc.source]
			if len(got) != 1 || got[0] != gapLedger {
				t.Errorf("%s: gap on %s attributed as %v, want exactly [%d] on %s",
					tc.source, contract, recBySource, gapLedger, tc.source)
			}
			ok, problems := sourceRecognitionOK(src.genesis, gapLedger, got, false, priorProjection{})
			if ok {
				t.Errorf("%s: recognition_ok stayed TRUE over an unhandled topic on %s (F071)", tc.source, contract)
			}
			if len(problems) != 1 || problems[0] != gapLedger {
				t.Errorf("%s: recognition problem ledgers = %v, want [%d] folded into the watermark",
					tc.source, problems, gapLedger)
			}
		}
	}
}

// TestCatalogue_RecognitionPinsMatchDecoderIdentity holds the F071 pins in
// step with each decoder's own identity gate. contractIDs is not only the
// recognition owner map: ch-rebuild and ch-reproject use it as a HARD
// per-event filter and the re-derive uses it as the lake prefilter. A pin
// naming a contract the decoder does not claim would attribute a
// stranger's gaps to this source; a pin MISSING a contract the decoder
// does claim would silently drop that contract's rows from a rebuild.
func TestCatalogue_RecognitionPinsMatchDecoderIdentity(t *testing.T) {
	ownerOf, cat := staticOwners(t)

	rozoSrc := catalogueSource(t, cat, "rozo")
	for _, c := range rozoSrc.contractIDs {
		if !rozo.IsRozoContract(c) {
			t.Errorf("rozo pins %s, which the rozo decoder does not claim", c)
		}
	}
	for _, c := range rozo.MainnetPaymentContracts {
		if ownerOf[c] != "rozo" {
			t.Errorf("rozo decoder claims %s but the catalogue attributes it to %q", c, ownerOf[c])
		}
	}
	if len(rozoSrc.contractIDs) != len(rozo.MainnetPaymentContracts) {
		t.Errorf("rozo pins %d contracts, decoder claims %d", len(rozoSrc.contractIDs), len(rozo.MainnetPaymentContracts))
	}

	bbSrc := catalogueSource(t, cat, blend_backstop.SourceName)
	for _, c := range bbSrc.contractIDs {
		if !blend_backstop.IsBackstopContract(c) {
			t.Errorf("blend_backstop pins %s, which the backstop decoder does not claim", c)
		}
	}
	for _, c := range []string{blend_backstop.MainnetBackstopV2, blend_backstop.MainnetBackstopV1} {
		if ownerOf[c] != blend_backstop.SourceName {
			t.Errorf("backstop decoder claims %s but the catalogue attributes it to %q", c, ownerOf[c])
		}
	}

	// No OTHER source may pin one of these contracts: ownerOf is
	// last-writer-wins, so a second pin would silently steal attribution.
	pinned := map[string]string{}
	for _, c := range rozoSrc.contractIDs {
		pinned[c] = "rozo"
	}
	for _, c := range bbSrc.contractIDs {
		pinned[c] = blend_backstop.SourceName
	}
	for _, src := range cat {
		for _, c := range src.contractIDs {
			if owner, ok := pinned[c]; ok && owner != src.name {
				t.Errorf("%s also pins %s, which belongs to %s — ownerOf would be last-writer-wins", src.name, c, owner)
			}
		}
	}

	// The catalogue must hold a copy, never the package's own slice.
	if len(rozoSrc.contractIDs) > 0 && &rozoSrc.contractIDs[0] == &rozo.MainnetPaymentContracts[0] {
		t.Error("rozo contractIDs aliases rozo.MainnetPaymentContracts — a catalogue-side append/sort would mutate the decoder's trust root")
	}
}
