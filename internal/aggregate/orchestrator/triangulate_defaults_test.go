package orchestrator

import (
	"os"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// The deployed triangulation chain table lives in the ansible template
// rather than in config.Default(), matching how this repo already
// layers coverage-set decisions: `[aggregate].pairs` defaults to EMPTY
// in config.Default() and the actual pair list is a deployment
// decision, because a chain is only meaningful for a deployment whose
// pair set contains its legs. A chain baked into config.Default() would
// emit permanent `missing_leg` metrics on any deployment with a
// different coverage set.
//
// That layering has a cost this test pays: nothing else in the build
// parses the template, so a typo in a pair string would surface as an
// aggregator that refuses to start (buildTriangulations fails loud) or,
// worse, as four chains quietly reporting missing_leg forever. These
// assertions run the template's own stanza through the real loader
// (config.AggregatorTriangulations) and the real structural validator.
const ansibleTemplatePath = "../../../configs/ansible/roles/archival-node/templates/stellarindex.toml.j2"

// extractTriangulationStanza pulls the `[[aggregate.triangulations]]`
// array-of-tables out of the jinja-templated config. The stanza itself
// is pure TOML (no jinja substitution), so it round-trips through the
// real decoder; the rest of the file does not, which is why this reads
// a slice rather than the whole document.
func extractTriangulationStanza(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(ansibleTemplatePath)
	if err != nil {
		t.Fatalf("read %s: %v", ansibleTemplatePath, err)
	}
	var out []string
	inStanza := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "[[aggregate.triangulations]]":
			inStanza = true
		case strings.HasPrefix(trimmed, "["):
			inStanza = false
			continue
		}
		if inStanza {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no [[aggregate.triangulations]] stanza in %s", ansibleTemplatePath)
	}
	return strings.Join(out, "\n")
}

// TestAnsibleTriangulationChains_AreValidAndComplete pins the deployed
// composite-rate chains: the four thin default pairs, each priced
// through the deep USD pivot plus an FX leg.
//
// Measured 2026-07-25 on the live index, share of minutes served from a
// SINGLE source: XLM/GBP 100%, XLM/EUR 87.5%, ETH/GBP 43.7%, BTC/GBP
// 25.7% — against XLM/USD and BTC/USD at 0.0%. Those four are the
// pairs a composite has something to add to.
func TestAnsibleTriangulationChains_AreValidAndComplete(t *testing.T) {
	var doc struct {
		Aggregate config.AggregateConfig `toml:"aggregate"`
	}
	if _, err := toml.Decode(extractTriangulationStanza(t), &doc); err != nil {
		t.Fatalf("decode triangulation stanza: %v", err)
	}

	resolved, err := doc.Aggregate.AggregatorTriangulations()
	if err != nil {
		t.Fatalf("AggregatorTriangulations: %v — the deployed stanza does not "+
			"parse through the loader the aggregator uses at boot", err)
	}

	want := map[string][]string{
		"crypto:XLM/fiat:EUR": {"crypto:XLM/fiat:USD", "fiat:USD/fiat:EUR"},
		"crypto:XLM/fiat:GBP": {"crypto:XLM/fiat:USD", "fiat:USD/fiat:GBP"},
		"crypto:ETH/fiat:GBP": {"crypto:ETH/fiat:USD", "fiat:USD/fiat:GBP"},
		"crypto:BTC/fiat:GBP": {"crypto:BTC/fiat:USD", "fiat:USD/fiat:GBP"},
	}
	if len(resolved) != len(want) {
		t.Fatalf("template declares %d chains, want %d", len(resolved), len(want))
	}

	for _, r := range resolved {
		target := r.Target.String()
		wantLegs, ok := want[target]
		if !ok {
			t.Errorf("unexpected chain target %q in the deployed template", target)
			continue
		}
		delete(want, target)

		chain := TriangulationChain{Target: r.Target, Legs: r.Legs}
		if err := ValidateTriangulationChain(chain); err != nil {
			// buildTriangulations fails the aggregator's boot on this,
			// so a bad row here is a deploy-time outage.
			t.Errorf("chain %s fails the boot-time validator: %v", target, err)
			continue
		}
		if len(r.Legs) != len(wantLegs) {
			t.Errorf("chain %s has %d legs, want %d", target, len(r.Legs), len(wantLegs))
			continue
		}
		for i, leg := range r.Legs {
			if leg.String() != wantLegs[i] {
				t.Errorf("chain %s leg[%d] = %q, want %q", target, i, leg.String(), wantLegs[i])
			}
		}

		// Leg-supply invariant, and the reason these chains can work at
		// all: the first leg is a crypto pair quoted in fiat:USD (the
		// aggregator caches its VWAP directly), and the second is
		// fiat/fiat — which has NO VWAP cache key anywhere, because no
		// fiat/fiat pair is aggregated. It resolves only through the
		// X2.5 forex-snap path (isFXLeg → Config.FXStore →
		// timescale.FXQuoteAtOrBefore → the fx_quotes table the
		// `massive` worker writes). If a future edit makes both legs
		// non-FX the chain silently stops publishing, so pin it.
		if isFXLeg(r.Legs[0]) {
			t.Errorf("chain %s: first leg %s is fiat/fiat; the crypto→USD leg must come first",
				target, r.Legs[0].String())
		}
		if !isFXLeg(r.Legs[len(r.Legs)-1]) {
			t.Errorf("chain %s: last leg %s is not an FX leg — a non-USD fiat target has no "+
				"other price source, since no fiat/fiat pair is aggregated into a VWAP cache key",
				target, r.Legs[len(r.Legs)-1].String())
		}
	}
	for missing := range want {
		t.Errorf("chain for %s missing from the deployed template", missing)
	}
}
