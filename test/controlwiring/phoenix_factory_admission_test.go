package controlwiring

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
)

// ─── F048: phoenix's factory anchor must be able to admit a pool ───
//
// GRADUATED out of the k023evidence build tag (it lived in
// deployed_controls_test.go) when the phoenix decoder learned to admit a
// factory-announced pool. Untagged so it runs in the default suite as
// this control's regression guard.
//
// pipeline.GatedMeta declares phoenix Factories + CreationSym "create",
// and seed-protocol-contracts walks exactly those events — but it only
// calls Decode on events the decoder Matches. Phoenix's Matches used to
// reject every factory event (classifyAny had no "create" action and
// reg.Has excludes the factory), so the walk and the live-upsert hook
// were provably inert: a pool the factory created was fail-closed until
// someone edited MainnetPools by hand.
//
// Driven by the REAL lake captures under test/fixtures/phoenix/
// factory-create (loader + shape pins: phoenix_factory_create_fixture_
// test.go). The assertion is ADMISSION, not recognition: every announced
// pool is already in the curated seed, so reg.Has proves nothing, but
// contractid.Registry.Seed fires its hook on every call — so the hook
// receiving (announced pool, factory, creation ledger) is the one
// observation only a decoder which really seeds from the event can
// produce. A patch that makes Matches accept the event and then drops it
// on the floor leaves this RED, as it should: the control would still be
// inert, and ch-recognition (one exemplar per (contract, topic_0_sym)
// shape, run through Matches) could then report the factory as
// recognised while nothing is admitted.
func TestK023_PhoenixFactoryCreateEventIsAdmissible(t *testing.T) {
	t.Parallel()
	type seeded struct {
		child, factory string
		ledger         uint32
	}
	var got []seeded
	dec := phoenix.NewDecoder(contractid.WithHook(func(child, factory string, ledger uint32) {
		got = append(got, seeded{child, factory, ledger})
	}))

	rows := phoenixCreateRows(t)
	var want []seeded
	for _, r := range rows {
		ev := r.event()
		announcedPool := phoenixAnnouncedPool(t, r)
		want = append(want, seeded{announcedPool, phoenix.MainnetFactory, r.LedgerSeq})
		if !dec.Matches(ev) {
			t.Errorf("ledger %d: phoenix.Decoder.Matches rejects the factory's real "+
				"(\"create\",\"liquidity_pool\") event announcing %s", r.LedgerSeq, announcedPool)
			continue
		}
		if _, err := dec.Decode(ev); err != nil {
			t.Errorf("ledger %d: decode factory create: %v", r.LedgerSeq, err)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("live-upsert hook fired %d times over %d real factory create events: "+
			"seed-protocol-contracts and the indexer can never admit a factory-created pool (F048)",
			len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("hook call %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestK023_PhoenixFactoryCreateFromForeignEmitterIsNotAdmitted is the
// property any F048 fix must keep: topic shape is forgeable, so the same
// real event republished by a contract that is NOT the factory must
// neither match nor seed. It is the security half of the pair: the fix
// that turns the test above green must not do it by trusting the topic
// alone, and the decoder's gate is reg.IsFactory for this action.
func TestK023_PhoenixFactoryCreateFromForeignEmitterIsNotAdmitted(t *testing.T) {
	t.Parallel()
	hooked := 0
	dec := phoenix.NewDecoder(contractid.WithHook(func(string, string, uint32) { hooked++ }))
	for _, r := range phoenixCreateRows(t) {
		ev := r.event()
		// A curated POOL is the strongest forger: it already passes reg.Has.
		ev.ContractID = phoenix.MainnetPools[0]
		if dec.Matches(ev) {
			t.Errorf("ledger %d: a non-factory emitter of (\"create\",\"liquidity_pool\") matches", r.LedgerSeq)
			_, _ = dec.Decode(ev)
		}
	}
	if hooked != 0 {
		t.Errorf("a non-factory emitter seeded the registry %d time(s)", hooked)
	}
}
