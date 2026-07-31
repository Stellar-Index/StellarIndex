package redstone

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// ─── 2026-07-31 attribution-chain hardening ────────────────────────────
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
	// subset arity into forcing the payload fallback (audit F2).
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

func TestSubsetFromStateWrites_DuplicateCandidatesCountWrittenFeedOnce(t *testing.T) {
	// Defence-in-depth below the ErrDuplicateFeedIDs gate: a repeated
	// candidate must not count the single written feed twice and
	// inflate the subset arity (audit F2).
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
