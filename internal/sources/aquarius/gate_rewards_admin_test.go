package aquarius

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// TestDecoder_MatchesRewards_gated pins that the rewards-gauge events
// (migration 0099, ROADMAP #89) are gated on contract identity
// IDENTICALLY to trade/liquidity/reserves: a REGISTERED pool matches,
// an unregistered look-alike emitting the exact same topic does not.
// Uses real captured bytes (see decode_rewards_test.go) for the topic
// shapes, swapping only the ContractID — Matches() never reads e.Value.
func TestDecoder_MatchesRewards_gated(t *testing.T) {
	d := NewDecoder()
	registered := MainnetPools[0]
	const foreign = "CFOREIGNFAKEPOOL0000000000000000000000000000000000000000"

	cases := []struct {
		name  string
		topic []string
	}{
		{"pool_state", []string{"AAAADwAAAApwb29sX3N0YXRlAAA="}},
		{"claim_reward", []string{
			"AAAADwAAAAxjbGFpbV9yZXdhcmQ=",
			"AAAAEgAAAAEohS9owZhIjjRvsSEu1QKQU3Ycwk9FM5LjU5ggGwgl5w==",
			"AAAAEgAAAAAAAAAAGFJvImUhe1Um7DcQIll44FVjzfnDHLalppun+3zFidQ=",
		}},
		{"rewards_gauge_add", []string{"AAAADwAAABFyZXdhcmRzX2dhdWdlX2FkZAAAAA=="}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !d.Matches(events.Event{ContractID: registered, Topic: tc.topic}) {
				t.Errorf("registered pool %s not matched for %s", registered, tc.name)
			}
			if d.Matches(events.Event{ContractID: foreign, Topic: tc.topic}) {
				t.Errorf("foreign contract matched for %s — CS-026 injection vector open", tc.name)
			}
		})
	}
}

// TestDecoder_MatchesAdmin_gated pins that the governance/upgrade
// events (migration 0100, ROADMAP #89) are gated on the CANONICAL
// ROUTER trust root only (not a registered pool, not an arbitrary
// contract) — see decode_admin.go's package doc for why the gate
// stops at the router and does not expand to the unidentified
// contract family real lake bytes show emitting several of these
// kinds alongside the flagged parallel router deployment.
func TestDecoder_MatchesAdmin_gated(t *testing.T) {
	d := NewDecoder()
	pool := MainnetPools[0]
	const flaggedRouter = "CA7RQDMMV6E53P5EDZA5GPWBZ33AMW2ZNO42XLI2RGRIAP4QXIARUOJQ"

	cases := []struct {
		name  string
		topic []string
	}{
		{"apply_upgrade", []string{"AAAADwAAAA1hcHBseV91cGdyYWRlAAAA"}},
		{"config_rewards", []string{
			"AAAADwAAAA5jb25maWdfcmV3YXJkcwAA",
			"AAAAEAAAAAEAAAACAAAAEgAAAAEBXYCbqoen8nj67TgxiToTyzhZ6BokJeLbYyJFVbtOGgAAABIAAAABJbT82FmuwvpjSEOMSJs8PBDJi20hvk/TyzDLaJU++Xc=",
		}},
		{"pool_gauge_switch_token", []string{
			"AAAADwAAABdwb29sX2dhdWdlX3N3aXRjaF90b2tlbgA=",
			"AAAAEgAAAAFQkI25aXl99CnhS5sIYZCU5/Wh49ZuSaRsWLA/BuP6Vg==",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !d.Matches(events.Event{ContractID: MainnetRouter, Topic: tc.topic}) {
				t.Errorf("canonical router not matched for %s", tc.name)
			}
			// A registered POOL must not match governance topics — the
			// trust root for this family is the router, not the pool
			// registry.
			if d.Matches(events.Event{ContractID: pool, Topic: tc.topic}) {
				t.Errorf("registered pool incorrectly matched governance topic %s", tc.name)
			}
			// The flagged parallel router deployment (real bytes show
			// it emitting these exact topics) must still fail-closed —
			// same CS-026 posture as its trade events.
			if d.Matches(events.Event{ContractID: flaggedRouter, Topic: tc.topic}) {
				t.Errorf("flagged parallel router matched %s — CS-026 gap not closed", tc.name)
			}
		})
	}
}

// TestDecoder_Decode_RewardsAndAdmin_endToEnd exercises Decode() (not
// just Matches()) for one representative kind from each new family,
// using real captured bytes, proving the dispatcher-facing seam wires
// decodeRewardsEvent / decodeAdminEvent correctly end-to-end.
func TestDecoder_Decode_RewardsAndAdmin_endToEnd(t *testing.T) {
	d := NewDecoder()
	closedAtStr := "2026-07-10T00:00:00Z"

	t.Run("rewards", func(t *testing.T) {
		out, err := d.Decode(events.Event{
			ContractID:     "CCFGZJTHQZGDZP5PK6WMLKHKJ72ACSVMJGCI2NFR7Q6EAVSKWLJB3ZH3",
			Ledger:         62000053,
			TxHash:         "3c3a180d0a7d467621df239a9370355e4e4249c8f98729f7163510dde8a80899",
			EventIndex:     1,
			LedgerClosedAt: closedAtStr,
			Topic: []string{
				"AAAADwAAAAxjbGFpbV9yZXdhcmQ=",
				"AAAAEgAAAAEohS9owZhIjjRvsSEu1QKQU3Ycwk9FM5LjU5ggGwgl5w==",
				"AAAAEgAAAAAAAAAAGFJvImUhe1Um7DcQIll44FVjzfnDHLalppun+3zFidQ=",
			},
			Value: "AAAAEAAAAAEAAAABAAAACgAAAAAAAAAAAAAADA0rT7g=",
		})
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if len(out) != 1 {
			t.Fatalf("got %d events, want 1", len(out))
		}
		rv, ok := out[0].(RewardsEvent)
		if !ok {
			t.Fatalf("got %T, want RewardsEvent", out[0])
		}
		if rv.Kind != RewardsClaimReward {
			t.Errorf("Kind = %q", rv.Kind)
		}
		if rv.EventKind() != "aquarius.rewards" {
			t.Errorf("EventKind() = %q", rv.EventKind())
		}
		if rv.Source() != SourceName {
			t.Errorf("Source() = %q", rv.Source())
		}
	})

	t.Run("admin", func(t *testing.T) {
		out, err := d.Decode(events.Event{
			ContractID:     MainnetRouter,
			Ledger:         59270084,
			TxHash:         "795ca1edf536361904eaf9e830f80766febb6564e5e8e76c1f81e1138c2db983",
			LedgerClosedAt: closedAtStr,
			Topic: []string{
				"AAAADwAAABdwb29sX2dhdWdlX3N3aXRjaF90b2tlbgA=",
				"AAAAEgAAAAFQkI25aXl99CnhS5sIYZCU5/Wh49ZuSaRsWLA/BuP6Vg==",
			},
			Value: "AAAAEAAAAAEAAAABAAAAAAAAAAE=",
		})
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if len(out) != 1 {
			t.Fatalf("got %d events, want 1", len(out))
		}
		av, ok := out[0].(AdminEvent)
		if !ok {
			t.Fatalf("got %T, want AdminEvent", out[0])
		}
		if av.Kind != AdminPoolGaugeSwitchToken {
			t.Errorf("Kind = %q", av.Kind)
		}
		if av.EventKind() != "aquarius.admin" {
			t.Errorf("EventKind() = %q", av.EventKind())
		}
		if av.Source() != SourceName {
			t.Errorf("Source() = %q", av.Source())
		}
	})
}
