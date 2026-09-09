package upshift

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// ─── Golden real-lake decode-VALUE tests ────────────────────────────
//
// CONVENTION (shared with internal/sources/defindex/golden_realbytes_test.go
// and internal/sources/aquarius/golden_realbytes_test.go):
//
// Each fixture below is the EXACT on-chain bytes of one real event —
// topics_xdr + data_xdr, base64 as r1's ClickHouse lake stores them in
// stellar.contract_events — captured 2026-09-09 and cited with the
// ledger_seq / tx_hash / op_index / event_index it came from, so any
// reader can re-pull and re-verify it. No hand-encoded SCVals.
//
// Every test drives the PRODUCTION decoder through both of its seams —
// Decoder.Matches (the ADR-0035 contract-identity gate) and
// Decoder.Decode (classify + body decode + consumer.Event wrap) — and
// asserts the exact decoded field VALUES. Recognition ("did Matches
// claim it?") and projection ("is the row COUNT right?") both pass for
// a decoder that puts WRONG values in the right number of rows; only a
// value assertion catches that.

// mustDecodeOne drives the production decoder and returns the single
// Event it emitted, failing if the gate rejects the fixture or the
// decoder emits any count other than one.
func mustDecodeOne(t *testing.T, ev events.Event) Event {
	t.Helper()
	d := NewDecoder()
	if !d.Matches(ev) {
		t.Fatalf("Matches = false for a real gated %s from %s — the fixture no longer passes the ADR-0035 gate",
			classify(&ev), ev.ContractID)
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("Decode emitted %d events, want exactly 1", len(out))
	}
	got, ok := out[0].(Event)
	if !ok {
		t.Fatalf("Decode emitted %T, want upshift.Event", out[0])
	}
	if got.EventKind() != EventKind || got.Source() != SourceName {
		t.Errorf("consumer.Event identity = (%q, %q), want (%q, %q)",
			got.EventKind(), got.Source(), EventKind, SourceName)
	}
	return got
}

// TestGolden_earnUSDCDeposit_ledger62938336 pins the very first
// earnUSDC deposit.
//
//	ledger_seq   62,938,336  (closed 2026-06-08 14:40:29 UTC)
//	tx_hash      e86b9a910f5e61db9b98fb60086dd495e6b2f2846cb806b2801a6f6d2d6c16f3
//	op_index 0, event_index 1
//	contract_id  CCL3WITW… (earnUSDC)
//	topics       [Symbol("deposit"), Address(G…), Address(G…), Address(G…)]
//	data         Map{ assets: i128=2000000, shares: i128=2000000000000 }
//
// The two amounts are the load-bearing assertion. They differ by
// EXACTLY 1,000,000 — the ERC-4626 decimals offset of 6 — so a decoder
// that read the same field twice, or swapped assets for shares, would
// produce a plausible-looking number and only an exact pin catches it.
// The three identical topic addresses are the shape 184 of 184 deposits
// carry (see TestGolden_earnUSDCWithdraw_ledger63812816 for the one
// event in either vault's history that separates them).
func TestGolden_earnUSDCDeposit_ledger62938336(t *testing.T) {
	t.Parallel()

	const (
		wantAddr   = "GC2ENBNFYPBWMGOIW4I2FX5ORVIQBNYMAHDND5G6FKN5RZZWVX4PQ24N"
		wantAssets = "2000000"
		wantShares = "2000000000000"
	)
	const topicAddr = "AAAAEgAAAAAAAAAAtEaFpcPDZhnItxGi366NUQC3DAHG0fTeKpvY5zat+Pg="

	got := mustDecodeOne(t, events.Event{
		Type:           "contract",
		ContractID:     MainnetVaultEarnUSDC,
		Ledger:         62938336,
		LedgerClosedAt: "2026-06-08T14:40:29Z",
		TxHash:         "e86b9a910f5e61db9b98fb60086dd495e6b2f2846cb806b2801a6f6d2d6c16f3",
		OperationIndex: 0,
		EventIndex:     1,
		Topic:          []string{TopicSymbolDeposit, topicAddr, topicAddr, topicAddr},
		Value: "AAAAEQAAAAEAAAACAAAADwAAAAZhc3NldHMAAAAAAAoAAAAAAAAAAAAAAAAAHoSA" +
			"AAAADwAAAAZzaGFyZXMAAAAAAAoAAAAAAAAAAAAAAdGpSiAA",
	})

	if got.Kind != EventDeposit {
		t.Errorf("Kind = %q, want %q", got.Kind, EventDeposit)
	}
	if got.Caller != wantAddr || got.Receiver != wantAddr || got.Owner != wantAddr {
		t.Errorf("addresses = (%q, %q, %q), want all %q",
			got.Caller, got.Receiver, got.Owner, wantAddr)
	}
	if got.Assets.String() != wantAssets {
		t.Errorf("Assets = %s, want %s", got.Assets, wantAssets)
	}
	if got.Shares.String() != wantShares {
		t.Errorf("Shares = %s, want %s", got.Shares, wantShares)
	}
	if !got.OldAmount.IsZero() || !got.NewAmount.IsZero() {
		t.Errorf("deployed-assets fields must stay zero on a deposit, got old=%s new=%s",
			got.OldAmount, got.NewAmount)
	}
	if got.ObservedAt.UTC().Format("2006-01-02T15:04:05Z") != "2026-06-08T14:40:29Z" {
		t.Errorf("ObservedAt = %s, want the ledger close time 2026-06-08T14:40:29Z", got.ObservedAt)
	}
	if got.EventIndex != 1 {
		t.Errorf("EventIndex = %d, want 1 — the SAC transfer occupies index 0 of the same op", got.EventIndex)
	}
}

// TestGolden_earnUSDCWithdraw_ledger63812816 pins ONE of the only two
// events in the two vaults' combined history whose three topic addresses
// differ — the pair that proves the (caller, receiver, owner) ordering.
// TestGolden_earnXLMWithdraw_ledger63812795 pins the other.
//
//	ledger_seq   63,812,816  (closed 2026-08-05 16:04:17 UTC)
//	tx_hash      2a008c6241e08a5d4788f173055d5fb98bdc86f2d427c2495947be935d14c7b0
//	op_index 0, event_index 5
//	contract_id  CCL3WITW… (earnUSDC)
//	topics       [Symbol("withdraw"), Address(C…), Address(G…), Address(C…)]
//	data         Map{ assets: i128=10000000, shares: i128=9919211002150 }
//
// Six ledgers earlier the SAME G account approved that C contract as a
// spender and transferred its shares to it
// (TestGolden_earnUSDCShareTransfer_ledger63812811 is that transfer), so
// at burn time the contract both CALLED and OWNED the shares while the
// G account received the underlying. Pinning Caller == Owner == C and
// Receiver == G is therefore a real assertion, not a tautology: swap
// topics 2 and 3 in the decoder and this test fails.
//
// The amounts also pin the share price. 9,919,211,002,150 shares
// redeem 10,000,000 assets — 0.8% BELOW the 1e6 genesis ratio, i.e. a
// share worth more than at launch. A decoder that read `shares` twice
// would report par and pass every count-based check.
func TestGolden_earnUSDCWithdraw_ledger63812816(t *testing.T) {
	t.Parallel()

	const (
		wantCallerOwner = "CCJ43PIDUBSVX4FAI3MJJTFWC3ZXLECARFNJUOUUTKHB5JP32Q75LVPN"
		wantReceiver    = "GCNC7GXVI2LYUNS7VMJXRDC57HV2LPX2PXXTSVQIJ6NFH5QQWNNIUUC7"
		wantAssets      = "10000000"
		wantShares      = "9919211002150"
	)

	got := mustDecodeOne(t, events.Event{
		Type:           "contract",
		ContractID:     MainnetVaultEarnUSDC,
		Ledger:         63812816,
		LedgerClosedAt: "2026-08-05T16:04:17Z",
		TxHash:         "2a008c6241e08a5d4788f173055d5fb98bdc86f2d427c2495947be935d14c7b0",
		OperationIndex: 0,
		EventIndex:     5,
		Topic: []string{
			TopicSymbolWithdraw,
			"AAAAEgAAAAGTzb0DoGVb8KBG2JTMthbzdZBAiVqaOpSajh6l+9Q/1Q==",
			"AAAAEgAAAAAAAAAAmi+a9UaXijZfqxN4jF3566W++n3vOVYIT5pT9hCzWoo=",
			"AAAAEgAAAAGTzb0DoGVb8KBG2JTMthbzdZBAiVqaOpSajh6l+9Q/1Q==",
		},
		Value: "AAAAEQAAAAEAAAACAAAADwAAAAZhc3NldHMAAAAAAAoAAAAAAAAAAAAAAAAAmJaA" +
			"AAAADwAAAAZzaGFyZXMAAAAAAAoAAAAAAAAAAAAACQV/DFkm",
	})

	if got.Kind != EventWithdraw {
		t.Errorf("Kind = %q, want %q", got.Kind, EventWithdraw)
	}
	if got.Caller != wantCallerOwner {
		t.Errorf("Caller = %q, want the invoking contract %q", got.Caller, wantCallerOwner)
	}
	if got.Receiver != wantReceiver {
		t.Errorf("Receiver = %q, want the account that got the underlying, %q", got.Receiver, wantReceiver)
	}
	if got.Owner != wantCallerOwner {
		t.Errorf("Owner = %q, want the share holder at burn time %q", got.Owner, wantCallerOwner)
	}
	if got.Assets.String() != wantAssets {
		t.Errorf("Assets = %s, want %s", got.Assets, wantAssets)
	}
	if got.Shares.String() != wantShares {
		t.Errorf("Shares = %s, want %s", got.Shares, wantShares)
	}
}

// TestGolden_earnUSDCShareTransfer_ledger63812811 pins a real share
// transfer — the SEP-41 event the vault emits for its OWN share token,
// in the CAP-67 Map body form.
//
//	ledger_seq   63,812,811  (closed 2026-08-05 16:03:50 UTC)
//	tx_hash      f2b68531502335f15cac27bcc9cb0709cf3fafb080db42a9ea7832ad44d31faf
//	op_index 0, event_index 0
//	contract_id  CCL3WITW… (earnUSDC)
//	topics       [Symbol("transfer"), Address(G…), Address(C…)]
//	data         Map{ amount: i128=99192108191209, to_muxed_id: Void }
//
// The body is the Map form, not a bare i128, and it carries a second
// field the decoder must ignore rather than choke on or mistake for the
// amount (`to_muxed_id`, Void here). The amount is 99,192,108,191,209 —
// ten times the shares the withdraw five ledgers later burns, which is
// exactly the kind of near-miss a positional or first-field read would
// return.
func TestGolden_earnUSDCShareTransfer_ledger63812811(t *testing.T) {
	t.Parallel()

	const (
		wantFrom   = "GCNC7GXVI2LYUNS7VMJXRDC57HV2LPX2PXXTSVQIJ6NFH5QQWNNIUUC7"
		wantTo     = "CCJ43PIDUBSVX4FAI3MJJTFWC3ZXLECARFNJUOUUTKHB5JP32Q75LVPN"
		wantAmount = "99192108191209"
	)

	got := mustDecodeOne(t, events.Event{
		Type:           "contract",
		ContractID:     MainnetVaultEarnUSDC,
		Ledger:         63812811,
		LedgerClosedAt: "2026-08-05T16:03:50Z",
		TxHash:         "f2b68531502335f15cac27bcc9cb0709cf3fafb080db42a9ea7832ad44d31faf",
		OperationIndex: 0,
		EventIndex:     0,
		Topic: []string{
			TopicSymbolTransfer,
			"AAAAEgAAAAAAAAAAmi+a9UaXijZfqxN4jF3566W++n3vOVYIT5pT9hCzWoo=",
			"AAAAEgAAAAGTzb0DoGVb8KBG2JTMthbzdZBAiVqaOpSajh6l+9Q/1Q==",
		},
		Value: "AAAAEQAAAAEAAAACAAAADwAAAAZhbW91bnQAAAAAAAoAAAAAAAAAAAAAWjb2X43p" +
			"AAAADwAAAAt0b19tdXhlZF9pZAAAAAAB",
	})

	if got.Kind != EventTransfer {
		t.Errorf("Kind = %q, want %q", got.Kind, EventTransfer)
	}
	if got.Caller != wantFrom {
		t.Errorf("Caller (from) = %q, want %q", got.Caller, wantFrom)
	}
	if got.Receiver != wantTo {
		t.Errorf("Receiver (to) = %q, want %q", got.Receiver, wantTo)
	}
	if got.Shares.String() != wantAmount {
		t.Errorf("Shares (transfer amount) = %s, want %s", got.Shares, wantAmount)
	}
	// A transfer moves an existing claim; it mints and redeems nothing.
	if !got.Assets.IsZero() {
		t.Errorf("Assets = %s on a share transfer, want zero — no underlying moves", got.Assets)
	}
	if got.Owner != "" {
		t.Errorf("Owner = %q on a share transfer, want empty — the event has only 3 topics", got.Owner)
	}
}

// TestGolden_earnUSDCDeployedAssets_ledger63069536 pins a real
// deployed-capital change.
//
//	ledger_seq   63,069,536  (closed 2026-06-17 10:42:46 UTC)
//	tx_hash      e096bdd6eb3ab6eb792c548b99bb480b4358057bc1fd6d65f31481f9cc1119e8
//	op_index 0, event_index 0
//	contract_id  CCL3WITW… (earnUSDC)
//	topics       [Symbol("deployed_assets_changed"), Address(G… operator)]
//	data         Map{ new_amount: i128=57037889700000, old_amount: i128=57000001000000 }
//
// The two amounts are adjacent in magnitude (they differ by 0.07%), so
// a decoder that read old into new — or read the map positionally,
// where `new_amount` is FIRST on the wire despite `old` being the
// logically prior value — still returns a number that looks right. Only
// pinning both exactly catches it. This fixture is therefore also the
// decode-by-name guard for this event.
func TestGolden_earnUSDCDeployedAssets_ledger63069536(t *testing.T) {
	t.Parallel()

	const (
		wantOperator = "GCDR6QB2LVRAKM6KMK46NYAT4M7I53S7N5ELEHNPQDIGJSLWYEPR76UH"
		wantOld      = "57000001000000"
		wantNew      = "57037889700000"
	)

	got := mustDecodeOne(t, events.Event{
		Type:           "contract",
		ContractID:     MainnetVaultEarnUSDC,
		Ledger:         63069536,
		LedgerClosedAt: "2026-06-17T10:42:46Z",
		TxHash:         "e096bdd6eb3ab6eb792c548b99bb480b4358057bc1fd6d65f31481f9cc1119e8",
		OperationIndex: 0,
		EventIndex:     0,
		Topic: []string{
			TopicSymbolDeployedAssetsChanged,
			"AAAAEgAAAAAAAAAAhx9AOl1iBTPKYrnm4BPjPo7uX29Ish2vgNBkyXbBHx8=",
		},
		Value: "AAAAEQAAAAEAAAACAAAADwAAAApuZXdfYW1vdW50AAAAAAAKAAAAAAAAAAAAADPg" +
			"KyeAoAAAAA8AAAAKb2xkX2Ftb3VudAAAAAAACgAAAAAAAAAAAAAz11jP0kA=",
	})

	if got.Kind != EventDeployedAssetsChanged {
		t.Errorf("Kind = %q, want %q", got.Kind, EventDeployedAssetsChanged)
	}
	if got.Caller != wantOperator {
		t.Errorf("Caller (operator) = %q, want %q", got.Caller, wantOperator)
	}
	if got.OldAmount.String() != wantOld {
		t.Errorf("OldAmount = %s, want %s", got.OldAmount, wantOld)
	}
	if got.NewAmount.String() != wantNew {
		t.Errorf("NewAmount = %s, want %s", got.NewAmount, wantNew)
	}
	if got.NewAmount.Cmp(got.OldAmount) <= 0 {
		t.Error("this fixture is a capital INCREASE; new_amount must exceed old_amount")
	}
	if !got.Assets.IsZero() || !got.Shares.IsZero() {
		t.Errorf("flow fields must stay zero, got assets=%s shares=%s", got.Assets, got.Shares)
	}
	if got.Receiver != "" || got.Owner != "" {
		t.Errorf("2-topic event must leave Receiver/Owner empty, got %q / %q", got.Receiver, got.Owner)
	}
}

// TestGolden_earnXLMDeposit_ledger62638654 pins the earnXLM vault's
// first deposit — the fixture that carries the second vault's identity.
//
//	ledger_seq   62,638,654  (closed 2026-05-19 11:27:09 UTC)
//	tx_hash      c6bcced312e54f601979055a325a7df5b14f742649a9de4a2fbeea33f1b245ce
//	op_index 0, event_index 1
//	contract_id  CC6TRAPQ… (earnXLM)
//	topics       [Symbol("deposit"), Address(G…), Address(G…), Address(G…)]
//	data         Map{ assets: i128=5000000, shares: i128=5000000000000 }
//
// Event index 0 of the SAME operation is a `transfer` from the native
// XLM SAC (CAS3J7GY…, topic[3] = String("native")) for amount 5000000 —
// byte-equal to this deposit's `assets`. That is the evidence that
// pins this vault's underlying to XLM, and hence its identity as
// earnXLM (see the package doc). Pinning `assets` here therefore also
// pins the join to that proof.
func TestGolden_earnXLMDeposit_ledger62638654(t *testing.T) {
	t.Parallel()

	const (
		wantAddr   = "GBSP45BMPDJMR7CXGUC7U73SPN5T5MFV4QKYPZKSFOFDZ6VTBB4ZW4SK"
		wantAssets = "5000000"
		wantShares = "5000000000000"
	)
	const topicAddr = "AAAAEgAAAAAAAAAAZP50LHjSyPxXNQX6f3J7ez6wteQVh+VSK4o8+rMIeZs="

	got := mustDecodeOne(t, events.Event{
		Type:           "contract",
		ContractID:     MainnetVaultEarnXLM,
		Ledger:         62638654,
		LedgerClosedAt: "2026-05-19T11:27:09Z",
		TxHash:         "c6bcced312e54f601979055a325a7df5b14f742649a9de4a2fbeea33f1b245ce",
		OperationIndex: 0,
		EventIndex:     1,
		Topic:          []string{TopicSymbolDeposit, topicAddr, topicAddr, topicAddr},
		Value: "AAAAEQAAAAEAAAACAAAADwAAAAZhc3NldHMAAAAAAAoAAAAAAAAAAAAAAAAATEtA" +
			"AAAADwAAAAZzaGFyZXMAAAAAAAoAAAAAAAAAAAAABIwnOVAA",
	})

	if got.ContractID != MainnetVaultEarnXLM {
		t.Errorf("ContractID = %q, want the earnXLM vault", got.ContractID)
	}
	if got.Kind != EventDeposit {
		t.Errorf("Kind = %q, want %q", got.Kind, EventDeposit)
	}
	if got.Caller != wantAddr {
		t.Errorf("Caller = %q, want %q", got.Caller, wantAddr)
	}
	if got.Assets.String() != wantAssets {
		t.Errorf("Assets = %s, want %s (byte-equal to the native-SAC transfer in the same op)",
			got.Assets, wantAssets)
	}
	if got.Shares.String() != wantShares {
		t.Errorf("Shares = %s, want %s", got.Shares, wantShares)
	}
}

// TestGolden_earnXLMWithdraw_ledger63045115 pins an earnXLM redemption.
//
//	ledger_seq   63,045,115  (closed 2026-06-15 19:15:08 UTC)
//	tx_hash      d1829de5c3af1b069b15d28b186d01329a8eee7a68e9a06fb524666f8059280d
//	op_index 0, event_index 1
//	contract_id  CC6TRAPQ… (earnXLM)
//	topics       [Symbol("withdraw"), Address(G…), Address(G…), Address(G…)]
//	data         Map{ assets: i128=20000000, shares: i128=20000000000000 }
//
// A withdraw AT PAR (exactly the 1e6 offset ratio), which is the
// complement to the earnUSDC withdraw fixture's above-par redemption:
// together they prove the decoder reads the two fields independently
// rather than deriving one from the other.
func TestGolden_earnXLMWithdraw_ledger63045115(t *testing.T) {
	t.Parallel()

	const (
		wantAddr   = "GCCZH4IW4RXYHORC7IXS3HOUTMFL54D7C5RQ5ZC5ZYXCIFEUSV4P6TKX"
		wantAssets = "20000000"
		wantShares = "20000000000000"
	)
	const topicAddr = "AAAAEgAAAAAAAAAAhZPxFuRvg7oi+i8tndSbCr7wfxdjDuRdzi4kFJSVeP8="

	got := mustDecodeOne(t, events.Event{
		Type:           "contract",
		ContractID:     MainnetVaultEarnXLM,
		Ledger:         63045115,
		LedgerClosedAt: "2026-06-15T19:15:08Z",
		TxHash:         "d1829de5c3af1b069b15d28b186d01329a8eee7a68e9a06fb524666f8059280d",
		OperationIndex: 0,
		EventIndex:     1,
		Topic:          []string{TopicSymbolWithdraw, topicAddr, topicAddr, topicAddr},
		Value: "AAAAEQAAAAEAAAACAAAADwAAAAZhc3NldHMAAAAAAAoAAAAAAAAAAAAAAAABMS0A" +
			"AAAADwAAAAZzaGFyZXMAAAAAAAoAAAAAAAAAAAAAEjCc5UAA",
	})

	if got.Kind != EventWithdraw {
		t.Errorf("Kind = %q, want %q", got.Kind, EventWithdraw)
	}
	if got.Owner != wantAddr {
		t.Errorf("Owner = %q, want %q", got.Owner, wantAddr)
	}
	if got.Assets.String() != wantAssets {
		t.Errorf("Assets = %s, want %s", got.Assets, wantAssets)
	}
	if got.Shares.String() != wantShares {
		t.Errorf("Shares = %s, want %s", got.Shares, wantShares)
	}
}

// TestGolden_earnXLMWithdraw_ledger63812795 pins the SECOND of the two
// events with three distinct topic addresses — the one that makes the
// (caller, receiver, owner) reading corroborated rather than a
// single-observation inference.
//
//	ledger_seq   63,812,795  (closed 2026-08-05 16:02:17 UTC)
//	tx_hash      6dbb49828778058bff42403d5c74d0687ba40750b7c2b7f664db18a075b72d3a
//	op_index 0, event_index 5
//	contract_id  CC6TRAPQ… (earnXLM)
//	topics       [Symbol("withdraw"), Address(C…), Address(G…), Address(C…)]
//	data         Map{ assets: i128=10000000, shares: i128=9995146356929 }
//
// The G account in the middle slot is BYTE-IDENTICAL to the one in the
// earnUSDC withdraw 21 ledgers later, while the outer contract is a
// DIFFERENT router. One holder redeeming both vaults in one session
// through two different routers is exactly the observation that
// separates "the middle slot is the receiver" from "the middle slot
// happens to be whatever that one transaction put there": the constant
// across the pair is the person, and the variable is the caller.
func TestGolden_earnXLMWithdraw_ledger63812795(t *testing.T) {
	t.Parallel()

	const (
		wantCallerOwner = "CD5YZRFQCATFOFZPWE4XDYJZMXAZSW5ROTIAO4D65Q7KTMZIEWGB7H7W"
		wantReceiver    = "GCNC7GXVI2LYUNS7VMJXRDC57HV2LPX2PXXTSVQIJ6NFH5QQWNNIUUC7"
		wantAssets      = "10000000"
		wantShares      = "9995146356929"
	)

	got := mustDecodeOne(t, events.Event{
		Type:           "contract",
		ContractID:     MainnetVaultEarnXLM,
		Ledger:         63812795,
		LedgerClosedAt: "2026-08-05T16:02:17Z",
		TxHash:         "6dbb49828778058bff42403d5c74d0687ba40750b7c2b7f664db18a075b72d3a",
		OperationIndex: 0,
		EventIndex:     5,
		Topic: []string{
			TopicSymbolWithdraw,
			"AAAAEgAAAAH7jMSwECZXFy+xOXHhOWXBmVuxdNAHcH7sPqmzKCWMHw==",
			"AAAAEgAAAAAAAAAAmi+a9UaXijZfqxN4jF3566W++n3vOVYIT5pT9hCzWoo=",
			"AAAAEgAAAAH7jMSwECZXFy+xOXHhOWXBmVuxdNAHcH7sPqmzKCWMHw==",
		},
		Value: "AAAAEQAAAAEAAAACAAAADwAAAAZhc3NldHMAAAAAAAoAAAAAAAAAAAAAAAAAmJaA" +
			"AAAADwAAAAZzaGFyZXMAAAAAAAoAAAAAAAAAAAAACRctJejB",
	})

	if got.Kind != EventWithdraw {
		t.Errorf("Kind = %q, want %q", got.Kind, EventWithdraw)
	}
	if got.Caller != wantCallerOwner || got.Owner != wantCallerOwner {
		t.Errorf("caller/owner = %q/%q, want both %q", got.Caller, got.Owner, wantCallerOwner)
	}
	if got.Receiver != wantReceiver {
		t.Errorf("Receiver = %q, want %q — the same holder as the earnUSDC withdraw 21 ledgers later",
			got.Receiver, wantReceiver)
	}
	if got.Caller == got.Receiver {
		t.Error("caller and receiver decoded equal; this fixture's whole value is that they differ")
	}
	if got.Assets.String() != wantAssets {
		t.Errorf("Assets = %s, want %s", got.Assets, wantAssets)
	}
	if got.Shares.String() != wantShares {
		t.Errorf("Shares = %s, want %s", got.Shares, wantShares)
	}
}

// TestGolden_foreignDepositIsNotClaimed is the ADR-0035 property, held
// against a REAL foreign event rather than a synthetic one.
//
//	ledger_seq   63,008,857  (closed 2026-06-13 08:43:41 UTC)
//	tx_hash      28a7846eccb82470909485fdafe1a37ca89cc67dfd7fb954ddd1c522bd1a1fc4
//	op_index 0, event_index 3
//	contract_id  CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7
//	topics       [Symbol("deposit"), Address(C…), Address(G…)]
//	data         Vec[ i128=62012497, i128=45073641 ]
//
// The collision is not hypothetical: a bounded 20,000-ledger census of
// the lake (63,000,000–63,020,000) found FOUR distinct contracts
// emitting `deposit` across 77 events, the vaults a minority of them.
// This is one of the others — byte-identical topic[0], a different
// protocol entirely.
//
// Two things are asserted, and the second is what makes the first
// meaningful:
//
//  1. Matches is FALSE for it, so the row is never attributed here.
//  2. classify() is NON-EMPTY for it — the topic alone DOES claim it.
//     Without this, assertion 1 could be passing for the wrong reason
//     (a topic mismatch), and the gate would be untested.
//
// The same fixture is then replayed with only the contract id changed
// to a real vault: Matches flips to true. Contract identity, and
// nothing else, is what decides.
func TestGolden_foreignDepositIsNotClaimed(t *testing.T) {
	t.Parallel()

	const foreignContract = "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7"

	ev := events.Event{
		Type:           "contract",
		ContractID:     foreignContract,
		Ledger:         63008857,
		LedgerClosedAt: "2026-06-13T08:43:41Z",
		TxHash:         "28a7846eccb82470909485fdafe1a37ca89cc67dfd7fb954ddd1c522bd1a1fc4",
		OperationIndex: 0,
		EventIndex:     3,
		Topic: []string{
			TopicSymbolDeposit,
			"AAAAEgAAAAESnMjMYzbx/bvcwPOYNDTDzbhf2eqFaXo3gtMY2HSlgA==",
			"AAAAEgAAAAAAAAAAzffM3k+0d5P8DWzZQtN8sqDdvoxopMRkJd8hadO96rQ=",
		},
		Value: "AAAAEAAAAAEAAAACAAAACgAAAAAAAAAAAAAAAAOwQFEAAAAKAAAAAAAAAAAAAAAAArLC6Q==",
	}

	// (2) first: the topic really does collide, so (1) is a gate result.
	if kind := classify(&ev); kind != EventDeposit {
		t.Fatalf("classify = %q for a real foreign `deposit`, want %q — this fixture no longer "+
			"exercises the collision the gate exists to stop", kind, EventDeposit)
	}

	d := NewDecoder()
	if d.Matches(ev) {
		t.Errorf("Matches = true for %s — a foreign contract's identical topic must NOT be "+
			"attributed to this protocol (ADR-0035)", foreignContract)
	}

	// The identical bytes from a real vault DO match: contract identity
	// is the only thing that changed.
	ev.ContractID = MainnetVaultEarnUSDC
	if !d.Matches(ev) {
		t.Error("Matches = false for the same bytes from a curated vault — the gate is rejecting " +
			"on something other than contract identity")
	}
}

// TestGolden_zeroEventAddressIsNotRegistered pins the address that
// circulated as this vault's but carries ZERO events of any kind in the
// lake. Indexing it would have created a second, empty identity for the
// same instrument — the asset-identity error that has cost this project
// a served surface before.
func TestGolden_zeroEventAddressIsNotRegistered(t *testing.T) {
	t.Parallel()

	const zeroEventAddr = "CC2DNHE5EFPPVJ47BJCRIQCEZ7KVHOATVR5QMNBEKKZQF2EZO7U7YTHT"

	if _, listed := MainnetVaults[zeroEventAddr]; listed {
		t.Fatalf("%s is in MainnetVaults; it has zero lake events and is not a vault", zeroEventAddr)
	}
	d := NewDecoder()
	for _, c := range d.GatedContractSet() {
		if c == zeroEventAddr {
			t.Fatalf("%s is in the gated set", zeroEventAddr)
		}
	}

	// A well-formed vault event that only differs by emitter is refused.
	const topicAddr = "AAAAEgAAAAAAAAAAtEaFpcPDZhnItxGi366NUQC3DAHG0fTeKpvY5zat+Pg="
	ev := events.Event{
		Type:           "contract",
		ContractID:     zeroEventAddr,
		Ledger:         62938336,
		LedgerClosedAt: "2026-06-08T14:40:29Z",
		TxHash:         "e86b9a910f5e61db9b98fb60086dd495e6b2f2846cb806b2801a6f6d2d6c16f3",
		Topic:          []string{TopicSymbolDeposit, topicAddr, topicAddr, topicAddr},
		Value: "AAAAEQAAAAEAAAACAAAADwAAAAZhc3NldHMAAAAAAAoAAAAAAAAAAAAAAAAAHoSA" +
			"AAAADwAAAAZzaGFyZXMAAAAAAAoAAAAAAAAAAAAAAdGpSiAA",
	}
	if d.Matches(ev) {
		t.Errorf("Matches = true for the zero-event address %s", zeroEventAddr)
	}
}
