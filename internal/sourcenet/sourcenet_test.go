package sourcenet

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// Pubnet never excludes anything — the default scope is byte-identical to
// the pre-#483 behaviour, which is the whole safety argument.
func TestApplicable_PubnetIsEverything(t *testing.T) {
	for _, src := range []string{"soroswap", "blend", "sdex", "recognition", "made-up-source"} {
		for _, net := range []string{"", Pubnet} {
			if ok, reason := Applicable(src, net); !ok || reason != "" {
				t.Errorf("Applicable(%q, %q) = %v %q, want true and no reason", src, net, ok, reason)
			}
		}
	}
}

func TestApplicable_TestNetsKeepOnlyLedgerAnchoredSources(t *testing.T) {
	for _, net := range []string{Testnet, Futurenet} {
		for _, src := range []string{"sdex", "sep41_transfers", "sep41_supply", "recognition"} {
			if ok, _ := Applicable(src, net); !ok {
				t.Errorf("%s must be applicable on %s", src, net)
			}
		}
		for _, src := range []string{
			"soroswap", "soroswap-router", "aquarius", "phoenix", "comet", "blend",
			"blend_backstop", "blend_emitter", "cctp", "rozo", "defindex", "sorocredit",
			"reflector-dex", "reflector-cex", "reflector-fx", "redstone", "band",
		} {
			ok, reason := Applicable(src, net)
			if ok {
				t.Errorf("%s must NOT be applicable on %s (pubnet contract identity)", src, net)
			}
			if !strings.Contains(reason, net) || !strings.Contains(reason, src) {
				t.Errorf("reason for %s/%s must name both: %q", src, net, reason)
			}
		}
	}
}

func TestFilter_SplitsAndSortsExcluded(t *testing.T) {
	in := []string{"soroswap", "sdex", "aquarius", "recognition", "blend"}
	app, exc := Filter(in, Testnet)
	if got, want := strings.Join(app, ","), "sdex,recognition"; got != want {
		t.Errorf("applicable = %q, want %q (input order preserved)", got, want)
	}
	if len(exc) != 3 || exc[0].Source != "aquarius" || exc[1].Source != "blend" || exc[2].Source != "soroswap" {
		t.Errorf("excluded = %+v, want aquarius, blend, soroswap sorted", exc)
	}
	app, exc = Filter(in, Pubnet)
	if len(app) != len(in) || len(exc) != 0 {
		t.Errorf("pubnet Filter must be the identity: %v / %v", app, exc)
	}
}

// Every pubnet-only source must be a name config.KnownSources accepts, and
// every KnownSources name must be classified (pubnet-only or all-network)
// — a new source added to one table and not the other fails here.
func TestPubnetOnlySources_MatchKnownSources(t *testing.T) {
	for _, s := range PubnetOnlySources {
		if _, ok := config.KnownSources[s]; !ok {
			t.Errorf("PubnetOnlySources lists %q, which config.KnownSources does not know", s)
		}
	}
	classified := map[string]struct{}{}
	for _, s := range PubnetOnlySources {
		classified[s] = struct{}{}
	}
	for s := range allNetworks {
		classified[s] = struct{}{}
	}
	for s := range config.KnownSources {
		if _, ok := classified[s]; !ok {
			t.Errorf("config.KnownSources has %q but sourcenet classifies it neither pubnet-only nor all-network", s)
		}
	}
	if got := NotApplicableOn(Pubnet); len(got) != 0 {
		t.Errorf("NotApplicableOn(pubnet) = %v, want empty", got)
	}
	if got := NotApplicableOn(Testnet); len(got) != len(PubnetOnlySources) {
		t.Errorf("NotApplicableOn(testnet) = %d entries, want %d", len(got), len(PubnetOnlySources))
	}
}
