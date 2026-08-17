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

// TestDecoder_MatchesAdmin_routerOnlyGated pins that the two
// ROUTER-SCOPED governance kinds (config_rewards, pool_gauge_switch_token
// — migration 0100, ROADMAP #89) stay gated on the CANONICAL ROUTER
// trust root only: a full-history r1 census (2026-08-17) finds ZERO
// pool-emitted occurrences of either, so a registered pool (and any
// arbitrary contract) must NOT match — see decode_admin.go's package
// doc.
func TestDecoder_MatchesAdmin_routerOnlyGated(t *testing.T) {
	d := NewDecoder()
	pool := MainnetPools[0]
	const flaggedRouter = "CA7RQDMMV6E53P5EDZA5GPWBZ33AMW2ZNO42XLI2RGRIAP4QXIARUOJQ"

	cases := []struct {
		name  string
		topic []string
	}{
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
			// A registered POOL must not match these two kinds — the
			// trust root for them is the router, not the pool registry
			// (zero pool-emitted occurrences in the lake).
			if d.Matches(events.Event{ContractID: pool, Topic: tc.topic}) {
				t.Errorf("registered pool incorrectly matched router-only topic %s", tc.name)
			}
			// The flagged parallel router deployment must still
			// fail-closed — same CS-026 posture as its trade events.
			if d.Matches(events.Event{ContractID: flaggedRouter, Topic: tc.topic}) {
				t.Errorf("flagged parallel router matched %s — CS-026 gap not closed", tc.name)
			}
		})
	}
}

// TestDecoder_MatchesPoolGovernance_recognized is the recognition
// regression for the pool-governance decoder gap (fix
// fix/aquarius-pool-governance-events). The seven POOL-EMITTABLE
// governance kinds — apply_upgrade, commit_upgrade, set_privileged_addrs,
// apply_transfer_ownership, commit_transfer_ownership,
// enable_emergency_mode, disable_emergency_mode — are legitimately
// emitted by the REGISTERED Aquarius pools (a protocol-wide staged WASM
// upgrade + pool-level ownership/emergency actions, ~1,679 real events).
// Before the fix the gate was reg.IsFactory ONLY, so every pool-emitted
// occurrence returned Matches()==false — an ADR-0033 recognition gap
// that also dropped the event from Decode. This pins that a REGISTERED
// pool AND the router now match, while a foreign contract and the
// flagged parallel router still fail-closed (CS-026). Matches() reads
// only topic[0], so a bare topic[0] proves the gate.
func TestDecoder_MatchesPoolGovernance_recognized(t *testing.T) {
	d := NewDecoder()
	pool := MainnetPools[0]
	const (
		foreign       = "CFOREIGNFAKEPOOL0000000000000000000000000000000000000000"
		flaggedRouter = "CA7RQDMMV6E53P5EDZA5GPWBZ33AMW2ZNO42XLI2RGRIAP4QXIARUOJQ"
	)

	cases := []struct {
		name   string
		topic0 string
	}{
		{"apply_upgrade", "AAAADwAAAA1hcHBseV91cGdyYWRlAAAA"},
		{"commit_upgrade", "AAAADwAAAA5jb21taXRfdXBncmFkZQAA"},
		{"set_privileged_addrs", "AAAADwAAABRzZXRfcHJpdmlsZWdlZF9hZGRycw=="},
		{"apply_transfer_ownership", "AAAADwAAABhhcHBseV90cmFuc2Zlcl9vd25lcnNoaXA="},
		{"commit_transfer_ownership", "AAAADwAAABljb21taXRfdHJhbnNmZXJfb3duZXJzaGlwAAAA"},
		{"enable_emergency_mode", "AAAADwAAABVlbmFibGVfZW1lcmdlbmN5X21vZGUAAAA="},
		{"disable_emergency_mode", "AAAADwAAABZkaXNhYmxlX2VtZXJnZW5jeV9tb2RlAAA="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			topic := []string{tc.topic0}
			// A registered pool must now be RECOGNIZED (gap closed).
			if !d.Matches(events.Event{ContractID: pool, Topic: topic}) {
				t.Errorf("registered pool NOT matched for %s — recognition gap still open", tc.name)
			}
			// The router still matches (its rows are unchanged).
			if !d.Matches(events.Event{ContractID: MainnetRouter, Topic: topic}) {
				t.Errorf("canonical router not matched for %s", tc.name)
			}
			// A foreign contract must still fail-closed (CS-026).
			if d.Matches(events.Event{ContractID: foreign, Topic: topic}) {
				t.Errorf("foreign contract matched %s — CS-026 injection vector open", tc.name)
			}
			// The flagged parallel router (neither reg.Has nor
			// reg.IsFactory) must still fail-closed.
			if d.Matches(events.Event{ContractID: flaggedRouter, Topic: topic}) {
				t.Errorf("flagged parallel router matched %s — CS-026 gap not closed", tc.name)
			}
		})
	}
}

// TestDecoder_Decode_PoolApplyUpgrade_endToEnd drives the full
// dispatcher seam (Matches gate → Decode → AdminEvent) for a REGISTERED
// pool's real 2-hash apply_upgrade, proving the recognition gap fix
// carries the event all the way to a projectable AdminEvent stamped with
// the POOL's contract_id (the aquarius_admin emitter column that
// distinguishes pool rows from router rows).
func TestDecoder_Decode_PoolApplyUpgrade_endToEnd(t *testing.T) {
	d := NewDecoder()
	const poolID = "CDKVJYMN34ZIEXSLNFYHVAFF6M6FM5E2U6OHXOTBKH2WLBULXOE53YDP"
	ev := events.Event{
		ContractID:     poolID,
		Ledger:         56505116,
		TxHash:         "f38a9207f724b9624ce39e15a1aef59197db4913bdc7cf74867db3906ef7a852",
		EventIndex:     2,
		LedgerClosedAt: "2025-04-07T08:42:31Z",
		Topic:          []string{"AAAADwAAAA1hcHBseV91cGdyYWRlAAAA"},
		Value:          "AAAAEAAAAAEAAAACAAAADQAAACAubx2u2HKIGsUr9jcygHbNgaO1pw5GUkRTo1QwnHo3hgAAAA0AAAAgWWrOi4VUNkeFEoIaLg7LApc7G60KQFfcVB/Qyk188Dc=",
	}
	if !d.Matches(ev) {
		t.Fatal("registered pool apply_upgrade not matched — recognition gap still open")
	}
	out, err := d.Decode(ev)
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
	if av.ContractID != poolID {
		t.Errorf("ContractID = %q, want the emitting pool %q", av.ContractID, poolID)
	}
	if av.Kind != AdminApplyUpgrade {
		t.Errorf("Kind = %q, want %q", av.Kind, AdminApplyUpgrade)
	}
	if av.Target != "2e6f1daed872881ac52bf637328076cd81a3b5a70e46524453a354309c7a3786" {
		t.Errorf("Target = %q", av.Target)
	}
	if got := av.Attributes["wasm_hash_1"]; got != "596ace8b855436478512821a2e0ecb02973b1bad0a4057dc541fd0ca4d7cf037" {
		t.Errorf("wasm_hash_1 = %v", got)
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
