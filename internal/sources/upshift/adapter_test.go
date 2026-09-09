package upshift

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// realDepositTopics is the topic vector of the real earnUSDC deposit at
// ledger 62,938,336 (see golden_realbytes_test.go). Reused wherever a
// test needs a well-formed event whose ONLY variable is the emitter.
func realDepositTopics() []string {
	const a = "AAAAEgAAAAAAAAAAtEaFpcPDZhnItxGi366NUQC3DAHG0fTeKpvY5zat+Pg="
	return []string{TopicSymbolDeposit, a, a, a}
}

const realDepositBody = "AAAAEQAAAAEAAAACAAAADwAAAAZhc3NldHMAAAAAAAoAAAAAAAAAAAAAAAAAHoSA" +
	"AAAADwAAAAZzaGFyZXMAAAAAAAoAAAAAAAAAAAAAAdGpSiAA"

func realDeposit(contractID string) events.Event {
	return events.Event{
		Type:           "contract",
		ContractID:     contractID,
		Ledger:         62938336,
		LedgerClosedAt: "2026-06-08T14:40:29Z",
		TxHash:         "e86b9a910f5e61db9b98fb60086dd495e6b2f2846cb806b2801a6f6d2d6c16f3",
		OperationIndex: 0,
		EventIndex:     1,
		Topic:          realDepositTopics(),
		Value:          realDepositBody,
	}
}

// TestTopicSymbols_MatchTheLakeBytes pins every pre-encoded topic Symbol
// to the exact base64 the lake stores in `topics_xdr[1]`, captured
// 2026-09-09. classify() compares these as BYTES, so a wrong encoding
// would silently make the decoder claim nothing at all — a failure that
// looks identical to "the protocol went quiet".
func TestTopicSymbols_MatchTheLakeBytes(t *testing.T) {
	t.Parallel()

	// Left: the constant. Right: the literal string observed in the lake.
	for _, tc := range []struct{ got, wantLakeBytes, sym string }{
		{TopicSymbolDeposit, "AAAADwAAAAdkZXBvc2l0AA==", EventDeposit},
		{TopicSymbolWithdraw, "AAAADwAAAAh3aXRoZHJhdw==", EventWithdraw},
		{TopicSymbolTransfer, "AAAADwAAAAh0cmFuc2Zlcg==", EventTransfer},
		{TopicSymbolDeployedAssetsChanged, "AAAADwAAABdkZXBsb3llZF9hc3NldHNfY2hhbmdlZAA=", EventDeployedAssetsChanged},
		{TopicSymbolApprove, "AAAADwAAAAdhcHByb3ZlAA==", EventApprove},
		{TopicSymbolDepositToSubaccount, "AAAADwAAABVkZXBvc2l0X3RvX3N1YmFjY291bnQAAAA=", EventDepositToSubaccount},
		{TopicSymbolWithdrawFromSub, "AAAADwAAABh3aXRoZHJhd19mcm9tX3N1YmFjY291bnQ=", EventWithdrawFromSubaccount},
		{TopicSymbolWalletDeployedUpd, "AAAADwAAABd3YWxsZXRfZGVwbG95ZWRfdXBkYXRlZAA=", EventWalletDeployedUpdated},
		{TopicSymbolWalletNetDeplSeeded, "AAAADwAAABp3YWxsZXRfbmV0X2RlcGxveWVkX3NlZWRlZAAA", EventWalletNetDeployedSeeded},
		{TopicSymbolSubaccountAdded, "AAAADwAAABBzdWJhY2NvdW50X2FkZGVk", EventSubaccountAdded},
		{TopicSymbolAdminSet, "AAAADwAAAAlhZG1pbl9zZXQAAAA=", EventAdminSet},
		{TopicSymbolOperatorSet, "AAAADwAAAAxvcGVyYXRvcl9zZXQ=", EventOperatorSet},
	} {
		if tc.got != tc.wantLakeBytes {
			t.Errorf("%s: encoded %q, lake stores %q", tc.sym, tc.got, tc.wantLakeBytes)
		}
		if tc.got != scval.MustEncodeSymbol(tc.sym) {
			t.Errorf("%s: constant is not the encoding of its own kind string", tc.sym)
		}
	}
}

// TestMainnetVaults_ShapeAndCompleteness pins the curated gate trust
// root: both vaults present, the zero-event address absent, every entry
// a plausible C-strkey with lake-verified metadata, and GenesisLedger
// equal to the EARLIEST vault's first event (it is the ADR-0031 density
// denominator — a rounded or late value silently inflates coverage).
func TestMainnetVaults_ShapeAndCompleteness(t *testing.T) {
	t.Parallel()

	if len(MainnetVaults) != 2 {
		t.Fatalf("MainnetVaults has %d entries, want 2 — a third vault needs the "+
			"bespoke-symbol lake sweep in the package doc re-run before it is admitted",
			len(MainnetVaults))
	}
	for _, want := range []string{MainnetVaultEarnUSDC, MainnetVaultEarnXLM} {
		meta, ok := MainnetVaults[want]
		if !ok {
			t.Fatalf("MainnetVaults is missing %s", want)
		}
		if meta.Label == "" || meta.Underlying == "" || meta.UnderlyingAsset == "" {
			t.Errorf("%s: incomplete metadata %+v", want, meta)
		}
		if !strings.HasPrefix(meta.Underlying, "C") || len(meta.Underlying) != 56 {
			t.Errorf("%s: Underlying %q is not a C-strkey", want, meta.Underlying)
		}
		if meta.FirstEventLedger < GenesisLedger {
			t.Errorf("%s: FirstEventLedger %d precedes GenesisLedger %d",
				want, meta.FirstEventLedger, GenesisLedger)
		}
	}

	var earliest uint32
	for _, m := range MainnetVaults {
		if earliest == 0 || m.FirstEventLedger < earliest {
			earliest = m.FirstEventLedger
		}
	}
	if GenesisLedger != earliest {
		t.Errorf("GenesisLedger = %d, want the earliest vault's first event %d", GenesisLedger, earliest)
	}

	// The gated set is exactly the curated map, deterministically ordered.
	gated := MainnetGatedSet()
	if len(gated) != len(MainnetVaults) {
		t.Fatalf("MainnetGatedSet has %d entries, MainnetVaults has %d", len(gated), len(MainnetVaults))
	}
	if !slices.IsSorted(gated) {
		t.Errorf("MainnetGatedSet is not sorted (%v) — the projector prefilter and the "+
			"reconcile lake read must be byte-stable across runs", gated)
	}
	for _, c := range gated {
		if _, ok := MainnetVaults[c]; !ok {
			t.Errorf("MainnetGatedSet carries %s, which is not a curated vault", c)
		}
	}
}

// TestVaultsHaveDistinctUnderlyings guards the identity error this
// project has already paid for once: two vaults collapsing onto one
// asset. earnUSDC and earnXLM are separate instruments holding separate
// underlyings, and neither share token is its underlying.
func TestVaultsHaveDistinctUnderlyings(t *testing.T) {
	t.Parallel()

	usdc := MainnetVaults[MainnetVaultEarnUSDC]
	xlm := MainnetVaults[MainnetVaultEarnXLM]
	if usdc.Underlying == xlm.Underlying {
		t.Errorf("both vaults claim underlying %s", usdc.Underlying)
	}
	for vault, meta := range MainnetVaults {
		if meta.Underlying == vault {
			t.Errorf("%s: the share token is recorded as its own underlying — a yield-bearing "+
				"claim is not the asset it is a claim on", vault)
		}
	}
}

// TestMatches_RejectsUnknownSymbolFromACuratedVault proves the gate is a
// conjunction, not a disjunction: being a curated vault is necessary but
// not sufficient. A symbol this protocol has never emitted stays
// unclaimed even from the real earnUSDC contract, so a future contract
// upgrade surfaces as a recognition gap rather than being decoded on a
// guess.
func TestMatches_RejectsUnknownSymbolFromACuratedVault(t *testing.T) {
	t.Parallel()

	ev := realDeposit(MainnetVaultEarnUSDC)
	ev.Topic = slices.Clone(ev.Topic)
	ev.Topic[0] = scval.MustEncodeSymbol("rebalance") // never emitted by either vault

	d := NewDecoder()
	if d.Matches(ev) {
		t.Error("Matches = true for a symbol neither vault emits")
	}
	if _, err := d.Decode(ev); !errors.Is(err, ErrNotUpshiftEvent) {
		t.Errorf("Decode error = %v, want ErrNotUpshiftEvent", err)
	}
}

// TestDecode_RecognizedCustodyEventsProjectZeroRows pins the ADR-0033
// contract for the eight events that are gated and classified but
// deliberately unserved: ZERO consumer.Events and NO error, so the
// re-derive counts their ledgers as expected-zero instead of marking
// them blind. Returning an error here is the INV-3 trap that held
// comet's completeness verdict open indefinitely.
func TestDecode_RecognizedCustodyEventsProjectZeroRows(t *testing.T) {
	t.Parallel()

	d := NewDecoder()
	for _, kind := range []string{
		EventApprove,
		EventDepositToSubaccount,
		EventWithdrawFromSubaccount,
		EventWalletDeployedUpdated,
		EventWalletNetDeployedSeeded,
		EventSubaccountAdded,
		EventAdminSet,
		EventOperatorSet,
	} {
		ev := realDeposit(MainnetVaultEarnUSDC)
		ev.Topic = slices.Clone(ev.Topic)
		ev.Topic[0] = scval.MustEncodeSymbol(kind)

		if !d.Matches(ev) {
			t.Errorf("%s: Matches = false — a recognized event must be claimed so its ledger "+
				"is counted, not left blind", kind)
			continue
		}
		out, err := d.Decode(ev)
		if err != nil {
			t.Errorf("%s: Decode error %v, want nil (recognized-but-unserved)", kind, err)
		}
		if len(out) != 0 {
			t.Errorf("%s: Decode emitted %d events, want 0", kind, len(out))
		}
	}
}

// TestDecodeTransfer_AcceptsTheBareI128Body covers the SEP-41 body form
// that no live vault event exercises.
//
// Both vaults launched after CAP-67, so every `transfer` either has
// emitted carries the Map form and the golden fixtures can only pin
// that one. The bare-i128 form is the ORIGINAL SEP-41 shape, and it is
// what a pre-Protocol-23 replay would present — so the branch that reads
// it must be exercised by something, or it is dead code that would only
// be discovered failing during a historical replay.
//
// The body below is real: it is a genuine pre-CAP-67 SEP-41 transfer
// body captured from the lake (contract CCH4KVAH…, ledger 52,000,000),
// carried here on a vault's topics because the shape, not the emitter,
// is what this test is about.
func TestDecodeTransfer_AcceptsTheBareI128Body(t *testing.T) {
	t.Parallel()

	const wantAmount = "78431372549"

	ev := events.Event{
		Type:           "contract",
		ContractID:     MainnetVaultEarnUSDC,
		Ledger:         63812811,
		LedgerClosedAt: "2026-08-05T16:03:50Z",
		TxHash:         "f2b68531502335f15cac27bcc9cb0709cf3fafb080db42a9ea7832ad44d31faf",
		Topic: []string{
			TopicSymbolTransfer,
			"AAAAEgAAAAAAAAAAmi+a9UaXijZfqxN4jF3566W++n3vOVYIT5pT9hCzWoo=",
			"AAAAEgAAAAGTzb0DoGVb8KBG2JTMthbzdZBAiVqaOpSajh6l+9Q/1Q==",
		},
		Value: "AAAACgAAAAAAAAAAAAAAEkLfxQU=", // bare i128, no Map wrapper
	}

	out, err := NewDecoder().Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("Decode emitted %d events, want 1", len(out))
	}
	got, ok := out[0].(Event)
	if !ok {
		t.Fatalf("Decode emitted %T, want upshift.Event", out[0])
	}
	if got.Kind != EventTransfer {
		t.Errorf("Kind = %q, want %q", got.Kind, EventTransfer)
	}
	if got.Shares.String() != wantAmount {
		t.Errorf("Shares = %s, want %s", got.Shares, wantAmount)
	}
}

// TestDecode_MalformedBodyIsAnError proves the complement of the test
// above: an INDETERMINATE body — one that did not decode — stays an
// error so the completeness verdict goes blind rather than silently
// under-counting. The zero-row path is reserved for events we fully
// understood and chose not to serve.
func TestDecode_MalformedBodyIsAnError(t *testing.T) {
	t.Parallel()

	d := NewDecoder()

	// A deposit whose body is the foreign contract's Vec, not a Map.
	ev := realDeposit(MainnetVaultEarnUSDC)
	ev.Value = "AAAAEAAAAAEAAAACAAAACgAAAAAAAAAAAAAAAAOwQFEAAAAKAAAAAAAAAAAAAAAAArLC6Q=="
	if _, err := d.Decode(ev); !errors.Is(err, ErrMalformedPayload) {
		t.Errorf("Vec body: error = %v, want ErrMalformedPayload", err)
	}

	// A deposit with the withdraw topic arity of a 3-topic event.
	ev = realDeposit(MainnetVaultEarnUSDC)
	ev.Topic = ev.Topic[:3]
	if _, err := d.Decode(ev); !errors.Is(err, ErrShortTopic) {
		t.Errorf("3 topics: error = %v, want ErrShortTopic", err)
	}

	// An unparseable ledger close time must fail closed, never fall back
	// to wall clock (which a backfill would stamp on every historical row).
	ev = realDeposit(MainnetVaultEarnUSDC)
	ev.LedgerClosedAt = "not-a-timestamp"
	if _, err := d.Decode(ev); !errors.Is(err, ErrMalformedPayload) {
		t.Errorf("bad close time: error = %v, want ErrMalformedPayload", err)
	}
}

// TestDecoder_DBWarmAdmitsAVaultWithoutARedeploy pins the operator seam
// the curated-set mechanism depends on (ADR-0040): a vault admitted via
// the protocol_contracts warm is gated IN without editing MainnetVaults.
// Without this, "operator-admitted" would be aspiration rather than a
// working path.
func TestDecoder_DBWarmAdmitsAVaultWithoutARedeploy(t *testing.T) {
	t.Parallel()

	const futureVault = "CDVBYETOFG7UYJAD6CMOAQZXBHEK3PD5ZDZKWMWIY5OXIWATPX4VGMY3"

	if NewDecoder().Matches(realDeposit(futureVault)) {
		t.Fatal("an un-admitted contract matched before the warm was applied")
	}
	warmed := NewDecoder(contractid.WithSeed([]string{futureVault}))
	if !warmed.Matches(realDeposit(futureVault)) {
		t.Error("Matches = false after the protocol_contracts warm admitted the contract")
	}
	// The curated pair survives the warm rather than being replaced by it.
	for _, c := range MainnetGatedSet() {
		if !slices.Contains(warmed.GatedContractSet(), c) {
			t.Errorf("curated vault %s dropped out of the gated set after a DB warm", c)
		}
	}
}

// TestGatedContractSet_IsTheMatchesDomain pins the invariant the
// projector's contract-id prefilter relies on: every contract in the
// gated set is one Matches can accept. If the two ever diverge, the
// prefilter drops events the decoder would have claimed — silent
// under-capture with no error anywhere.
func TestGatedContractSet_IsTheMatchesDomain(t *testing.T) {
	t.Parallel()

	d := NewDecoder()
	gated := d.GatedContractSet()
	if len(gated) == 0 {
		t.Fatal("GatedContractSet is empty — the projector would stream nothing")
	}
	for _, c := range gated {
		if !d.Matches(realDeposit(c)) {
			t.Errorf("%s is in the gated set but Matches rejects its events", c)
		}
	}
	if d.Name() != SourceName {
		t.Errorf("Name = %q, want %q", d.Name(), SourceName)
	}
}
