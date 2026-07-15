package defindex

import (
	"encoding/base64"
	"errors"
	"math/big"
	"testing"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"

	"github.com/Stellar-Index/StellarIndex/internal/contractid"
)

// TestClassify_depositWithdraw covers the topic-byte equality path —
// ensures topic[0] = ScvString("BlendStrategy") + topic[1] in
// {deposit, withdraw} is the only thing the decoder picks up.
// Verifies the byte-equality constants line up with the SDK encoder.
func TestClassify_depositWithdraw(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		topic     []string
		wantClass string
	}{
		{
			name:      "deposit",
			topic:     []string{TopicPrefixStrategy, TopicSymbolDeposit},
			wantClass: EventDeposit,
		},
		{
			name:      "withdraw",
			topic:     []string{TopicPrefixStrategy, TopicSymbolWithdraw},
			wantClass: EventWithdraw,
		},
		{
			name:      "wrong prefix (SoroswapPair)",
			topic:     []string{mustB64String(t, "SoroswapPair"), TopicSymbolDeposit},
			wantClass: "",
		},
		{
			name:      "prefix as Symbol not String",
			topic:     []string{mustB64Symbol(t, "BlendStrategy"), TopicSymbolDeposit},
			wantClass: "",
		},
		{
			// EVERY-event policy (2026-05-27): harvest is now classified
			// even though we don't produce a StrategyFlow for it yet.
			name:      "harvest (classification-only)",
			topic:     []string{TopicPrefixStrategy, TopicSymbolHarvest},
			wantClass: EventHarvest,
		},
		{
			name:      "single-element topic",
			topic:     []string{TopicPrefixStrategy},
			wantClass: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := &events.Event{Topic: tc.topic}
			got := classify(ev)
			if got != tc.wantClass {
				t.Errorf("classify = %q, want %q", got, tc.wantClass)
			}
		})
	}
}

// TestDecodeFlow_deposit covers the happy-path decode of a deposit
// event with an account (G-strkey) `from`. Verifies amount
// preservation (no truncation per ADR-0003) and address round-trip.
func TestDecodeFlow_deposit(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Type:           "contract",
		Ledger:         60_000_000,
		LedgerClosedAt: "2026-05-14T10:30:00Z",
		ContractID:     "CDB2WMKQQNVZMEBY7Q7GZ5C7E7IAFSNMZ7GGVD6WKTCEWK7XOIAVZSAP",
		OperationIndex: 2,
		TxHash:         "abc123",
		Topic:          []string{TopicPrefixStrategy, TopicSymbolDeposit},
		Value: mustB64(t, mapSCVal(t,
			mapEntry(t, "from", addrSCVal(makeAccountAddress(t, 0xAA))),
			mapEntry(t, "amount", i128SCVal(big.NewInt(123_456_789_000))),
		)),
	}
	flow, err := decodeFlow(ev, EventDeposit)
	if err != nil {
		t.Fatalf("decodeFlow: %v", err)
	}
	if flow.Source != SourceName {
		t.Errorf("Source = %q, want %q", flow.Source, SourceName)
	}
	if flow.Direction != DirectionDeposit {
		t.Errorf("Direction = %q, want deposit", flow.Direction)
	}
	if flow.From == "" || flow.From[0] != 'G' {
		t.Errorf("From = %q, want a G-strkey account address", flow.From)
	}
	if got, want := flow.Amount.String(), "123456789000"; got != want {
		t.Errorf("Amount = %q, want %q (no truncation)", got, want)
	}
	if flow.Ledger != 60_000_000 || flow.OpIndex != 2 || flow.TxHash != "abc123" {
		t.Errorf("header fields not preserved: %+v", flow)
	}
}

// TestDecodeFlow_withdrawFromContract covers the withdraw branch
// AND the real-world case where `from` is the vault/router
// *contract* (a C-strkey), not an end-user account — exactly what
// scan-soroban-events observed on mainnet. The body shape is
// identical to deposit; only Direction differs.
func TestDecodeFlow_withdrawFromContract(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Type:           "contract",
		Ledger:         60_000_001,
		LedgerClosedAt: "2026-05-14T10:31:00Z",
		ContractID:     "CC5CE6MWISDXT3MLNQ7R3FVILFVFEIH3COWGH45GJKL6BD2ZHF7F7JVI",
		Topic:          []string{TopicPrefixStrategy, TopicSymbolWithdraw},
		Value: mustB64(t, mapSCVal(t,
			mapEntry(t, "from", addrSCVal(makeContractAddress(t, 0xBB))),
			mapEntry(t, "amount", i128SCVal(big.NewInt(29_999_999))),
		)),
	}
	flow, err := decodeFlow(ev, EventWithdraw)
	if err != nil {
		t.Fatalf("decodeFlow: %v", err)
	}
	if flow.Direction != DirectionWithdraw {
		t.Errorf("Direction = %q, want withdraw", flow.Direction)
	}
	if flow.From == "" || flow.From[0] != 'C' {
		t.Errorf("From = %q, want a C-strkey contract address", flow.From)
	}
	if got, want := flow.Amount.String(), "29999999"; got != want {
		t.Errorf("Amount = %q, want %q", got, want)
	}
}

// TestDecodeFlow_missingField covers the malformed-input path. A
// body missing `amount` must return ErrMalformedPayload, not panic
// on a nil-deref.
func TestDecodeFlow_missingField(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     "CDB2WMKQQNVZMEBY7Q7GZ5C7E7IAFSNMZ7GGVD6WKTCEWK7XOIAVZSAP",
		LedgerClosedAt: "2026-05-14T10:30:00Z",
		Topic:          []string{TopicPrefixStrategy, TopicSymbolDeposit},
		Value: mustB64(t, mapSCVal(t,
			mapEntry(t, "from", addrSCVal(makeAccountAddress(t, 0xAA))),
			// no amount
		)),
	}
	_, err := decodeFlow(ev, EventDeposit)
	if !errors.Is(err, ErrMalformedPayload) {
		t.Errorf("err = %v, want ErrMalformedPayload", err)
	}
}

// TestDecodeFlow_badKind defends the defensive default branch — a
// kind classify() would never return must still error cleanly.
func TestDecodeFlow_badKind(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		LedgerClosedAt: "2026-05-14T10:30:00Z",
		Topic:          []string{TopicPrefixStrategy, TopicSymbolDeposit},
		Value:          mustB64(t, mapSCVal(t)),
	}
	_, err := decodeFlow(ev, "rebalance")
	if !errors.Is(err, ErrUnknownEvent) {
		t.Errorf("err = %v, want ErrUnknownEvent", err)
	}
}

// ─── Phase B (vault layer) tests ──────────────────────────────

// TestClassifyVault_depositWithdraw mirrors TestClassify_depositWithdraw
// for the vault-wrapper topic prefix. Topic[1] symbols are shared
// between strategy + vault layers (`deposit` / `withdraw`), so the
// reject paths here mainly cover topic[0] discrimination.
func TestClassifyVault_depositWithdraw(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		topic     []string
		wantClass string
	}{
		{
			name:      "vault deposit",
			topic:     []string{TopicPrefixVault, TopicSymbolDeposit},
			wantClass: EventDeposit,
		},
		{
			name:      "vault withdraw",
			topic:     []string{TopicPrefixVault, TopicSymbolWithdraw},
			wantClass: EventWithdraw,
		},
		{
			name:      "strategy prefix routes to classify(), not classifyVault()",
			topic:     []string{TopicPrefixStrategy, TopicSymbolDeposit},
			wantClass: "",
		},
		{
			name:      "vault prefix encoded as Symbol not String",
			topic:     []string{mustB64Symbol(t, "DeFindexVault"), TopicSymbolDeposit},
			wantClass: "",
		},
		// EVERY-event policy (2026-05-27): the nine vault governance /
		// admin / multiplexed-rebalance topics are now classified
		// (still no decoder — classification only). Pre-policy these
		// returned "" and got silently dropped.
		{name: "vault rescue", topic: []string{TopicPrefixVault, TopicSymbolRescue}, wantClass: EventRescue},
		{name: "vault paused", topic: []string{TopicPrefixVault, TopicSymbolPaused}, wantClass: EventPaused},
		{name: "vault unpaused", topic: []string{TopicPrefixVault, TopicSymbolUnpaused}, wantClass: EventUnpaused},
		{name: "vault nreceiver", topic: []string{TopicPrefixVault, TopicSymbolNReceiver}, wantClass: EventNReceiver},
		{name: "vault nmanager", topic: []string{TopicPrefixVault, TopicSymbolNManager}, wantClass: EventNManager},
		{name: "vault nemanager", topic: []string{TopicPrefixVault, TopicSymbolNEManager}, wantClass: EventNEManager},
		{name: "vault rbmanager", topic: []string{TopicPrefixVault, TopicSymbolRBManager}, wantClass: EventRBManager},
		{name: "vault dfees", topic: []string{TopicPrefixVault, TopicSymbolDFees}, wantClass: EventDFees},
		{name: "vault rebalance (multiplexed body)", topic: []string{TopicPrefixVault, TopicSymbolRebalance}, wantClass: EventRebalance},
		// ROADMAP #89 residual (2026-07-10): n_wasm — a read-only lake
		// topic census found 2 real occurrences classifyVault didn't
		// recognize. Classification-only (no decoder), same as the
		// other 9 admin topics above — the topic encoding itself is
		// verified (scval.MustEncodeSymbol, same mechanism the whole
		// package relies on), but a real-lake-bytes body sample was not
		// pulled: three separate ClickHouse queries (contract-scoped,
		// ledger-range-scoped, and topic-only) each timed out past
		// 400s against the raw 233M-row contract_events table without
		// a skip index on non-contract_id predicates — see
		// internal/sources/defindex/events.go's EventNWasm doc.
		{name: "vault n_wasm", topic: []string{TopicPrefixVault, TopicSymbolNWasm}, wantClass: EventNWasm},
		{
			name:      "single-element topic",
			topic:     []string{TopicPrefixVault},
			wantClass: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := &events.Event{Topic: tc.topic}
			got := classifyVault(ev)
			if got != tc.wantClass {
				t.Errorf("classifyVault = %q, want %q", got, tc.wantClass)
			}
		})
	}
}

// TestClassifyFactory_createNfee covers the factory layer added per
// EVERY-event policy (project_every_event_principle). Factory events
// are classified-only — Decode returns (nil, nil) on a factory match
// so the dispatcher's drop-counter doesn't file them as unmatched.
func TestClassifyFactory_createNfee(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		topic     []string
		wantClass string
	}{
		{
			name:      "factory create",
			topic:     []string{TopicPrefixFactory, TopicSymbolCreate},
			wantClass: EventCreate,
		},
		{
			name:      "factory n_fee",
			topic:     []string{TopicPrefixFactory, TopicSymbolNFee},
			wantClass: EventNFee,
		},
		{
			name:      "strategy prefix routes to classify(), not classifyFactory()",
			topic:     []string{TopicPrefixStrategy, TopicSymbolCreate},
			wantClass: "",
		},
		{
			name:      "vault prefix routes to classifyVault(), not classifyFactory()",
			topic:     []string{TopicPrefixVault, TopicSymbolCreate},
			wantClass: "",
		},
		{
			name:      "factory prefix encoded as Symbol not String",
			topic:     []string{mustB64Symbol(t, "DeFindexFactory"), TopicSymbolCreate},
			wantClass: "",
		},
		{
			name:      "factory with deposit symbol (wrong topic[1])",
			topic:     []string{TopicPrefixFactory, TopicSymbolDeposit},
			wantClass: "",
		},
		{
			name:      "single-element topic",
			topic:     []string{TopicPrefixFactory},
			wantClass: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := &events.Event{Topic: tc.topic}
			got := classifyFactory(ev)
			if got != tc.wantClass {
				t.Errorf("classifyFactory = %q, want %q", got, tc.wantClass)
			}
		})
	}
}

// TestDecode_factoryEvent_isClassifiedButEmits0Events verifies that
// the dispatcher's Decode entrypoint returns (nil, nil) — not the
// `ErrUnknownEvent` sentinel — for a factory match. This is the
// closed-loop completeness check: Matches() returns true → Decode()
// returns no error and no events, the event is consumed cleanly
// rather than recorded as an unmatched-topic drop. Uses `n_fee`
// (never body-decoded, so a bare Value is fine) — the `create` path's
// body decode is covered separately by
// TestDecode_factoryCreate_seedsStrategiesFromRealLakeBytes since
// ROADMAP #7 (2026-07-10) made `create` parse its body.
func TestDecode_factoryEvent_isClassifiedButEmits0Events(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	// The gate (ADR-0035/0040) only honours factory events from the
	// canonical trust roots — a create from a foreign contract is
	// rejected (TestDecoder_GateRejectsForeignContract).
	ev := events.Event{ContractID: MainnetFactories[0], Topic: []string{TopicPrefixFactory, TopicSymbolNFee}}
	if !d.Matches(ev) {
		t.Fatal("Matches(factory n_fee) = false, want true")
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Errorf("Decode(factory n_fee) err = %v, want nil", err)
	}
	if len(out) != 0 {
		t.Errorf("Decode(factory n_fee) emitted %d events, want 0", len(out))
	}
}

// ─── Factory `create` → strategy fan-out (ROADMAP #7, 2026-07-10) ──
//
// Real lake bytes (data_xdr) captured 2026-07-10 via ClickHouse HTTP
// against r1's certified raw lake, contract-scoped to the 3
// create-emitting DeFindexFactory instances. Each constant is one
// full `("DeFindexFactory","create")` event body, byte-identical to
// what the contract emitted on-chain.
const (
	// createBodyTwoStrategies — CDKFHFJI… (current factory), ledger
	// 57,057,068. One asset with TWO strategies:
	// CDB2WMKQQNVZMEBY7Q7GZ5C7E7IAFSNMZ7GGVD6WKTCEWK7XOIAVZSAP
	// ("blend_autocompound_fixed") and
	// CCSRX5E4337QMCMC3KO3RDFYI57T5NZV5XB3W3TWE4USCASKGL5URKJL
	// ("blend_autocompound_yieldblox") — both members of
	// MainnetStrategies.
	createBodyTwoStrategies = "AAAAEQAAAAEAAAADAAAADwAAAAZhc3NldHMAAAAAABAAAAABAAAAAQAAABEAAAABAAAAAgAAAA8AAAAHYWRkcmVzcwAAAAASAAAAAa3vzlmu5Slo92Bh1JTCUlt1ZZ+kKWpl9JnvKeVkd+SWAAAADwAAAApzdHJhdGVnaWVzAAAAAAAQAAAAAQAAAAIAAAARAAAAAQAAAAMAAAAPAAAAB2FkZHJlc3MAAAAAEgAAAAHDqzFQg2uWEDj8Pmz0XyfQAsmsz8xqj9ZUxEsr93IBXAAAAA8AAAAEbmFtZQAAAA4AAAAYYmxlbmRfYXV0b2NvbXBvdW5kX2ZpeGVkAAAADwAAAAZwYXVzZWQAAAAAAAAAAAAAAAAAEQAAAAEAAAADAAAADwAAAAdhZGRyZXNzAAAAABIAAAABpRv0nN7/BgmC2p24jLhHfz63Ne3Du252JykhAkoy+0gAAAAPAAAABG5hbWUAAAAOAAAAHGJsZW5kX2F1dG9jb21wb3VuZF95aWVsZGJsb3gAAAAPAAAABnBhdXNlZAAAAAAAAAAAAAAAAAAPAAAABXJvbGVzAAAAAAAAEQAAAAEAAAAEAAAAAwAAAAAAAAASAAAAAAAAAAA/yG0JmrdjpOcWUQkJHLRLd1OhvkvZDYFcHc7gVBDUmQAAAAMAAAABAAAAEgAAAAAAAAAAixJCFtWLc+peA9dQXbhNguV6nHi4456Q+b2VWsg3JIUAAAADAAAAAgAAABIAAAAAAAAAAJ8DBa6Ko1Zw7Uo5qB28HTW2ZtZrKsggNIY4eX8/F0FiAAAAAwAAAAMAAAASAAAAAAAAAAANx5WIC2/uT2FiHgSp7KMg/li5+cX+rFbIaNgZKoQ7ygAAAA8AAAAJdmF1bHRfZmVlAAAAAAAAAwAAB9A="

	// createBodyZeroStrategies — CDKFHFJI…, ledger 57,147,588. One
	// asset with an EMPTY strategies Vec — a legitimate, observed
	// on-chain shape (a vault created with no strategy attached yet),
	// not malformed.
	createBodyZeroStrategies = "AAAAEQAAAAEAAAADAAAADwAAAAZhc3NldHMAAAAAABAAAAABAAAAAQAAABEAAAABAAAAAgAAAA8AAAAHYWRkcmVzcwAAAAASAAAAASAi1W4KumRRb25iYE0pYjK+hk/9+4TVhhPnQjys4CsoAAAADwAAAApzdHJhdGVnaWVzAAAAAAAQAAAAAQAAAAAAAAAPAAAABXJvbGVzAAAAAAAAEQAAAAEAAAAEAAAAAwAAAAAAAAASAAAAAAAAAABuCGdDDiAqa8Ozjwj2jTBN1K57+trQBkkwYN0L5b4o6AAAAAMAAAABAAAAEgAAAAAAAAAAbghnQw4gKmvDs48I9o0wTdSue/ra0AZJMGDdC+W+KOgAAAADAAAAAgAAABIAAAAAAAAAAG4IZ0MOICprw7OPCPaNME3Urnv62tAGSTBg3QvlvijoAAAAAwAAAAMAAAASAAAAAAAAAABuCGdDDiAqa8Ozjwj2jTBN1K57+trQBkkwYN0L5b4o6AAAAA8AAAAJdmF1bHRfZmVlAAAAAAAAAwAAB9A="

	// createBodyEarliestFactory — CAVP2QLP… (the earliest of the 4
	// factories), ledger 55,484,403. One asset with ONE strategy:
	// CBTX63BX2I6E2VG2SMFQXDHLAPDOANUWBTMXQNWBV2FT6DIMVQPCSOBW
	// ("Blend Strategy") — confirms the field layout is
	// byte-identical across the factory-era history, not just on the
	// current factory.
	createBodyEarliestFactory = "AAAAEQAAAAEAAAADAAAADwAAAAZhc3NldHMAAAAAABAAAAABAAAAAQAAABEAAAABAAAAAgAAAA8AAAAHYWRkcmVzcwAAAAASAAAAASW0/NhZrsL6Y0hDjEibPDwQyYttIb5P08swy2iVPvl3AAAADwAAAApzdHJhdGVnaWVzAAAAAAAQAAAAAQAAAAEAAAARAAAAAQAAAAMAAAAPAAAAB2FkZHJlc3MAAAAAEgAAAAFnf2w30jxNVNqTCwuM6wPG4DaWDNl4NsGuiz8NDKweKQAAAA8AAAAEbmFtZQAAAA4AAAAOQmxlbmQgU3RyYXRlZ3kAAAAAAA8AAAAGcGF1c2VkAAAAAAAAAAAAAAAAAA8AAAAFcm9sZXMAAAAAAAARAAAAAQAAAAQAAAADAAAAAAAAABIAAAAAAAAAAI/sKanankkaQEGC08WiRi97yjWn3C73URmgU+eSxFGvAAAAAwAAAAEAAAASAAAAAAAAAACP7Cmp2p5JGkBBgtPFokYve8o1p9wu91EZoFPnksRRrwAAAAMAAAACAAAAEgAAAAAAAAAAj+wpqdqeSRpAQYLTxaJGL3vKNafcLvdRGaBT55LEUa8AAAADAAAAAwAAABIAAAAAAAAAAI/sKanankkaQEGC08WiRi97yjWn3C73URmgU+eSxFGvAAAADwAAAAl2YXVsdF9mZWUAAAAAAAADAAAAZA=="
)

// TestDecode_factoryCreate_seedsStrategiesFromRealLakeBytes pins the
// live strategy fan-out (ROADMAP #7, 2026-07-10): a `create` event
// from a canonical factory trust root Seeds every strategy address
// its body announces, and the strategy immediately passes Matches()
// for its own BlendStrategy flow topics — no operator step, no code
// change, matching the Blend/Soroswap/Aquarius fan-out pattern.
func TestDecode_factoryCreate_seedsStrategiesFromRealLakeBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		factory    string
		body       string
		wantSeeded []string
	}{
		{
			name:    "two strategies (current factory)",
			factory: "CDKFHFJIET3A73A2YN4KV7NSV32S6YGQMUFH3DNJXLBWL4SKEGVRNFKI",
			body:    createBodyTwoStrategies,
			wantSeeded: []string{
				"CDB2WMKQQNVZMEBY7Q7GZ5C7E7IAFSNMZ7GGVD6WKTCEWK7XOIAVZSAP",
				"CCSRX5E4337QMCMC3KO3RDFYI57T5NZV5XB3W3TWE4USCASKGL5URKJL",
			},
		},
		{
			name:       "zero strategies is legitimate, not malformed",
			factory:    "CDKFHFJIET3A73A2YN4KV7NSV32S6YGQMUFH3DNJXLBWL4SKEGVRNFKI",
			body:       createBodyZeroStrategies,
			wantSeeded: nil,
		},
		{
			name:       "earliest factory — same field layout",
			factory:    "CAVP2QLPIG7FQNHI57KXF7KS6NIAAUQKHZZDM3AGVADE64WHFBC5YURX",
			body:       createBodyEarliestFactory,
			wantSeeded: []string{"CBTX63BX2I6E2VG2SMFQXDHLAPDOANUWBTMXQNWBV2FT6DIMVQPCSOBW"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := NewDecoder() // fresh registry per case — curated set already contains these, so use a bare (unseeded) child gate to prove the SEED, not the curated fallback
			d.reg = contractid.New(contractid.WithFactories(MainnetFactories))

			ev := events.Event{
				Ledger:     60_000_000,
				ContractID: tc.factory,
				Topic:      []string{TopicPrefixFactory, TopicSymbolCreate},
				Value:      tc.body,
			}
			if !d.Matches(ev) {
				t.Fatal("Matches(create) = false, want true (canonical factory)")
			}
			out, err := d.Decode(ev)
			if err != nil {
				t.Fatalf("Decode(create) err = %v, want nil", err)
			}
			if len(out) != 0 {
				t.Errorf("Decode(create) emitted %d consumer.Event(s), want 0", len(out))
			}
			for _, want := range tc.wantSeeded {
				if !d.reg.Has(want) {
					t.Errorf("strategy %s not seeded after Decode(create)", want)
				}
				// The newly-seeded strategy must now pass its own
				// BlendStrategy flow gate — the whole point of fan-out.
				flowEv := events.Event{ContractID: want, Topic: []string{TopicPrefixStrategy, TopicSymbolDeposit}}
				if !d.Matches(flowEv) {
					t.Errorf("strategy %s seeded but its own deposit topic still fails to match", want)
				}
			}
		})
	}
}

// TestDecode_factoryCreate_malformedBodyErrors pins that a `create`
// event whose body doesn't have the expected top-level `assets` Map
// field is a genuine decode error (ErrMalformedPayload) — unlike
// harvest/rebalance/admin topics, which are recognised-but-unmodelled
// and must NOT error (BACKLOG #58 policy).
func TestDecode_factoryCreate_malformedBodyErrors(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	ev := events.Event{
		ContractID: MainnetFactories[0],
		Topic:      []string{TopicPrefixFactory, TopicSymbolCreate},
		Value:      mustB64(t, i128SCVal(big.NewInt(1))), // not a Map at all
	}
	if !d.Matches(ev) {
		t.Fatal("Matches(create) = false, want true")
	}
	_, err := d.Decode(ev)
	if !errors.Is(err, ErrMalformedPayload) {
		t.Errorf("Decode(malformed create) err = %v, want ErrMalformedPayload", err)
	}
}

// TestDecodeVaultFlow_deposit covers the happy path for a vault
// deposit event with the audit-doc body schema: a G-strkey
// `depositor`, a single-element `amounts` Vec<i128>, and
// `df_tokens_minted` i128. Verifies the User strkey round-trips,
// amounts preserve precision (ADR-0003), and direction is set.
func TestDecodeVaultFlow_deposit(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Type:           "contract",
		Ledger:         60_500_000,
		LedgerClosedAt: "2026-05-15T08:00:00Z",
		ContractID:     "CCA2ZJP5BVRXYTQH4FAGHCAUMRYCXVC4CRYC2NXHWMR7TIVX36U7F5HR",
		OperationIndex: 1,
		TxHash:         "vault-dep-abc",
		Topic:          []string{TopicPrefixVault, TopicSymbolDeposit},
		Value: mustB64(t, mapSCVal(t,
			mapEntry(t, "amounts", vecSCVal(t, i128SCVal(big.NewInt(10_000_000)))),
			mapEntry(t, "depositor", addrSCVal(makeAccountAddress(t, 0xCC))),
			mapEntry(t, "df_tokens_minted", i128SCVal(big.NewInt(9_876_543))),
		)),
	}
	flow, err := decodeVaultFlow(ev, EventDeposit)
	if err != nil {
		t.Fatalf("decodeVaultFlow: %v", err)
	}
	if flow.Source != SourceName {
		t.Errorf("Source = %q, want %q", flow.Source, SourceName)
	}
	if flow.Direction != DirectionDeposit {
		t.Errorf("Direction = %q, want deposit", flow.Direction)
	}
	if flow.User == "" || flow.User[0] != 'G' {
		t.Errorf("User = %q, want a G-strkey depositor", flow.User)
	}
	if got, want := len(flow.Amounts), 1; got != want {
		t.Fatalf("len(Amounts) = %d, want %d", got, want)
	}
	if got, want := flow.Amounts[0].String(), "10000000"; got != want {
		t.Errorf("Amounts[0] = %q, want %q", got, want)
	}
	if got, want := flow.DfTokens.String(), "9876543"; got != want {
		t.Errorf("DfTokens = %q, want %q", got, want)
	}
	if flow.Ledger != 60_500_000 || flow.OpIndex != 1 || flow.TxHash != "vault-dep-abc" {
		t.Errorf("header fields not preserved: %+v", flow)
	}
}

// TestDecodeVaultFlow_withdraw covers the withdraw branch and the
// per-direction field-name swap (`withdrawer` / `amounts_withdrawn`
// / `df_tokens_burned`). Body has a multi-asset amounts vec to
// confirm the Vec-decode loop handles >1 element.
func TestDecodeVaultFlow_withdraw(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Type:           "contract",
		Ledger:         60_500_001,
		LedgerClosedAt: "2026-05-15T08:01:00Z",
		ContractID:     "CCA2ZJP5BVRXYTQH4FAGHCAUMRYCXVC4CRYC2NXHWMR7TIVX36U7F5HR",
		Topic:          []string{TopicPrefixVault, TopicSymbolWithdraw},
		Value: mustB64(t, mapSCVal(t,
			// Two-asset basket exercises the Vec loop.
			mapEntry(t, "amounts_withdrawn", vecSCVal(t,
				i128SCVal(big.NewInt(5_000_000)),
				i128SCVal(big.NewInt(2_500_000)),
			)),
			mapEntry(t, "df_tokens_burned", i128SCVal(big.NewInt(7_400_000))),
			mapEntry(t, "withdrawer", addrSCVal(makeAccountAddress(t, 0xDD))),
		)),
	}
	flow, err := decodeVaultFlow(ev, EventWithdraw)
	if err != nil {
		t.Fatalf("decodeVaultFlow: %v", err)
	}
	if flow.Direction != DirectionWithdraw {
		t.Errorf("Direction = %q, want withdraw", flow.Direction)
	}
	if flow.User == "" || flow.User[0] != 'G' {
		t.Errorf("User = %q, want a G-strkey withdrawer", flow.User)
	}
	if got, want := len(flow.Amounts), 2; got != want {
		t.Fatalf("len(Amounts) = %d, want %d (multi-asset vec)", got, want)
	}
	if flow.Amounts[0].String() != "5000000" || flow.Amounts[1].String() != "2500000" {
		t.Errorf("Amounts = [%s, %s], want [5000000, 2500000]",
			flow.Amounts[0], flow.Amounts[1])
	}
	if got, want := flow.DfTokens.String(), "7400000"; got != want {
		t.Errorf("DfTokens = %q, want %q", got, want)
	}
}

// TestDecodeVaultFlow_routerDepositorContract confirms the
// occasional case where the depositor is a router/aggregator
// C-strkey (e.g. coming via a Soroswap-route into the vault) rather
// than a direct user G-strkey. Both decode the same way; only the
// User prefix differs.
func TestDecodeVaultFlow_routerDepositorContract(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Ledger:         60_500_002,
		LedgerClosedAt: "2026-05-15T08:02:00Z",
		ContractID:     "CCA2ZJP5BVRXYTQH4FAGHCAUMRYCXVC4CRYC2NXHWMR7TIVX36U7F5HR",
		Topic:          []string{TopicPrefixVault, TopicSymbolDeposit},
		Value: mustB64(t, mapSCVal(t,
			mapEntry(t, "amounts", vecSCVal(t, i128SCVal(big.NewInt(1_111_111)))),
			mapEntry(t, "depositor", addrSCVal(makeContractAddress(t, 0xEE))),
			mapEntry(t, "df_tokens_minted", i128SCVal(big.NewInt(1_000_000))),
		)),
	}
	flow, err := decodeVaultFlow(ev, EventDeposit)
	if err != nil {
		t.Fatalf("decodeVaultFlow: %v", err)
	}
	if flow.User == "" || flow.User[0] != 'C' {
		t.Errorf("User = %q, want a C-strkey contract address", flow.User)
	}
}

// TestDecodeVaultFlow_missingField defends the malformed-input
// path. The vault body has more required fields than the strategy
// body, so we explicitly verify the per-direction field names get
// surfaced in the error.
func TestDecodeVaultFlow_missingField(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     "CCA2ZJP5BVRXYTQH4FAGHCAUMRYCXVC4CRYC2NXHWMR7TIVX36U7F5HR",
		LedgerClosedAt: "2026-05-15T08:00:00Z",
		Topic:          []string{TopicPrefixVault, TopicSymbolDeposit},
		Value: mustB64(t, mapSCVal(t,
			mapEntry(t, "depositor", addrSCVal(makeAccountAddress(t, 0xCC))),
			// no amounts, no df_tokens_minted
		)),
	}
	_, err := decodeVaultFlow(ev, EventDeposit)
	if !errors.Is(err, ErrMalformedPayload) {
		t.Errorf("err = %v, want ErrMalformedPayload", err)
	}
}

// TestDecodeVaultFlow_emptyAmountsVec covers the degenerate but
// valid case of a zero-asset deposit — the Vec is empty rather
// than missing. Empty Vec is legal SCVal and the decoder accepts
// it (downstream consumers can decide what to do with no flow).
func TestDecodeVaultFlow_emptyAmountsVec(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     "CCA2ZJP5BVRXYTQH4FAGHCAUMRYCXVC4CRYC2NXHWMR7TIVX36U7F5HR",
		LedgerClosedAt: "2026-05-15T08:00:00Z",
		Topic:          []string{TopicPrefixVault, TopicSymbolDeposit},
		Value: mustB64(t, mapSCVal(t,
			mapEntry(t, "amounts", vecSCVal(t)),
			mapEntry(t, "depositor", addrSCVal(makeAccountAddress(t, 0xCC))),
			mapEntry(t, "df_tokens_minted", i128SCVal(big.NewInt(0))),
		)),
	}
	flow, err := decodeVaultFlow(ev, EventDeposit)
	if err != nil {
		t.Fatalf("decodeVaultFlow: %v", err)
	}
	if len(flow.Amounts) != 0 {
		t.Errorf("len(Amounts) = %d, want 0", len(flow.Amounts))
	}
}

// vecSCVal builds a Vec<ScVal>. Mirrors the helper in
// internal/scval/scval_test.go (kept here rather than DRYed because
// the production package doesn't export test builders).
func vecSCVal(t *testing.T, elts ...sdkxdr.ScVal) sdkxdr.ScVal {
	t.Helper()
	vec := sdkxdr.ScVec(elts)
	pv := &vec
	return sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvVec, Vec: &pv}
}

// ─── SCVal builders for tests ─────────────────────────────────
// Mirrored from internal/sources/soroswap_router/decode_test.go —
// keeping per-package builders rather than DRYing into a shared
// test helper because the test-time graph stays small + the
// builders are pure Go (no production dependencies to manage).

func i128SCVal(n *big.Int) sdkxdr.ScVal {
	abs := new(big.Int).Set(n)
	if abs.Sign() < 0 {
		abs.Neg(abs)
	}
	bytes := abs.Bytes()
	for len(bytes) < 16 {
		bytes = append([]byte{0}, bytes...)
	}
	hi := int64(0)
	for i := 0; i < 8; i++ {
		hi = (hi << 8) | int64(bytes[i])
	}
	lo := uint64(0)
	for i := 8; i < 16; i++ {
		lo = (lo << 8) | uint64(bytes[i])
	}
	if n.Sign() < 0 {
		hi = ^hi
		lo = ^lo + 1
		if lo == 0 {
			hi++
		}
	}
	return sdkxdr.ScVal{
		Type: sdkxdr.ScValTypeScvI128,
		I128: &sdkxdr.Int128Parts{
			Hi: sdkxdr.Int64(hi),
			Lo: sdkxdr.Uint64(lo),
		},
	}
}

func addrSCVal(addr sdkxdr.ScAddress) sdkxdr.ScVal {
	return sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvAddress, Address: &addr}
}

func makeAccountAddress(t *testing.T, fillByte byte) sdkxdr.ScAddress {
	t.Helper()
	var ed25519 sdkxdr.Uint256
	for i := range ed25519 {
		ed25519[i] = fillByte
	}
	acct := sdkxdr.AccountId{
		Type:    sdkxdr.PublicKeyTypePublicKeyTypeEd25519,
		Ed25519: &ed25519,
	}
	return sdkxdr.ScAddress{Type: sdkxdr.ScAddressTypeScAddressTypeAccount, AccountId: &acct}
}

func makeContractAddress(t *testing.T, fillByte byte) sdkxdr.ScAddress {
	t.Helper()
	var cid sdkxdr.ContractId
	for i := range cid {
		cid[i] = fillByte
	}
	return sdkxdr.ScAddress{Type: sdkxdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
}

func mapEntry(t *testing.T, key string, val sdkxdr.ScVal) sdkxdr.ScMapEntry {
	t.Helper()
	sym := sdkxdr.ScSymbol(key)
	keySv := sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvSymbol, Sym: &sym}
	return sdkxdr.ScMapEntry{Key: keySv, Val: val}
}

func mapSCVal(t *testing.T, entries ...sdkxdr.ScMapEntry) sdkxdr.ScVal {
	t.Helper()
	m := sdkxdr.ScMap(entries)
	pm := &m
	return sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvMap, Map: &pm}
}

func mustB64(t *testing.T, sv sdkxdr.ScVal) string {
	t.Helper()
	bs, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal scval: %v", err)
	}
	return base64.StdEncoding.EncodeToString(bs)
}

func mustB64String(t *testing.T, s string) string {
	t.Helper()
	xs := sdkxdr.ScString(s)
	sv := sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvString, Str: &xs}
	return mustB64(t, sv)
}

func mustB64Symbol(t *testing.T, s string) string {
	t.Helper()
	sym := sdkxdr.ScSymbol(s)
	sv := sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvSymbol, Sym: &sym}
	return mustB64(t, sv)
}

// symSCVal builds a Symbol ScVal (the raw value, not base64) for use
// as a map entry value — the sibling of mustB64Symbol which returns
// the encoded topic string.
func symSCVal(s string) sdkxdr.ScVal {
	sym := sdkxdr.ScSymbol(s)
	return sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvSymbol, Sym: &sym}
}

// TestDecoder_GateRejectsForeignContract pins ADR-0035/0040 (CS-026):
// the namespaced DeFindexVault/BlendStrategy topic strings are still
// just strings any pubnet contract can emit — the r1 lake contains
// emitters carrying the exact topic shape with NONE of the four
// DeFindex-provenance proofs (docs/protocols/defindex.md, flagged
// set). A perfect topic shape from an unregistered contract must NOT
// be attributed to defindex; the same event from a curated vault /
// strategy / factory must.
func TestDecoder_GateRejectsForeignContract(t *testing.T) {
	t.Parallel()
	d := NewDecoder() // production gate: curated evidence-verified set only

	vaultTopics := []string{TopicPrefixVault, TopicSymbolDeposit}
	strategyTopics := []string{TopicPrefixStrategy, TopicSymbolDeposit}
	factoryTopics := []string{TopicPrefixFactory, TopicSymbolCreate}

	foreign := "CFOREIGNFAKEVAULT000000000000000000000000000000000000000"
	for name, ev := range map[string]events.Event{
		"vault shape":    {ContractID: foreign, Topic: vaultTopics},
		"strategy shape": {ContractID: foreign, Topic: strategyTopics},
		"factory shape":  {ContractID: foreign, Topic: factoryTopics},
	} {
		if d.Matches(ev) {
			t.Fatalf("foreign contract with defindex-shaped topics (%s) matched — the CS-026 injection vector is open", name)
		}
	}

	// One flagged real-world example (docs/protocols/defindex.md):
	// carries the DeFindexVault topic shape but none of the four
	// provenance proofs — must stay excluded until verified.
	flagged := events.Event{
		ContractID: "CBGCGVKHVA4TG6MGQ3XTOEHEJXK4DYLOKTMR4UT4PZFPTQKLYXYRF6KV",
		Topic:      vaultTopics,
	}
	if d.Matches(flagged) {
		t.Fatal("flagged unverified emitter matched — it must fail-close into a recognition gap")
	}

	if !d.Matches(events.Event{ContractID: MainnetVaults[0], Topic: vaultTopics}) {
		t.Fatal("curated vault failed to match — gate is over-closed")
	}
	if !d.Matches(events.Event{ContractID: MainnetStrategies[0], Topic: strategyTopics}) {
		t.Fatal("curated strategy failed to match — gate is over-closed")
	}
	for _, f := range MainnetFactories {
		if !d.Matches(events.Event{ContractID: f, Topic: factoryTopics}) {
			t.Fatalf("canonical factory %s failed to match", f)
		}
	}
	// A factory is a trust root, not a child: vault-shaped events
	// from a factory address are NOT flows.
	if d.Matches(events.Event{ContractID: MainnetFactories[0], Topic: vaultTopics}) {
		t.Fatal("factory address matched a vault flow shape — factory and child sets must stay separate")
	}
}

// TestDecoder_OperatorSeedAdmitsNewVault pins the operator unblock
// path: a newly verified vault is admitted via the protocol_contracts
// warm (contractid.WithSeed) with NO code change.
func TestDecoder_OperatorSeedAdmitsNewVault(t *testing.T) {
	t.Parallel()
	newVault := "CNEWLYVERIFIEDVAULT0000000000000000000000000000000000000"
	ev := events.Event{ContractID: newVault, Topic: []string{TopicPrefixVault, TopicSymbolDeposit}}

	if NewDecoder().Matches(ev) {
		t.Fatal("unseeded vault matched")
	}
	if !NewDecoder(contractid.WithSeed([]string{newVault})).Matches(ev) {
		t.Fatal("protocol_contracts-seeded vault failed to match")
	}
}

// ─── Phase-B follow-up: harvest / rebalance / admin (BACKLOG #58) ──

// TestDecode_strategyHarvestRecognisedEmits0Events pins the harvest
// contract: a ("BlendStrategy","harvest") from a registered strategy
// is recognised (Matches true) but drops cleanly — Decode returns
// (nil, nil), NOT the ErrUnknownEvent sentinel it returned before
// #58. Filing harvest as a decode error inflated the source's
// decode-error counter for normal upstream yield events.
func TestDecode_strategyHarvestRecognisedEmits0Events(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	ev := events.Event{
		ContractID: MainnetStrategies[0],
		Topic:      []string{TopicPrefixStrategy, TopicSymbolHarvest},
	}
	if !d.Matches(ev) {
		t.Fatal("Matches(strategy harvest) = false, want true")
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Errorf("Decode(strategy harvest) err = %v, want nil (recognised, unmodelled — not a decode error)", err)
	}
	if len(out) != 0 {
		t.Errorf("Decode(strategy harvest) emitted %d events, want 0", len(out))
	}
}

// TestDecode_vaultRebalanceAndAdminRecognisedEmit0Events pins the
// vault-layer clean-drop contract across the rebalance topic and all
// eight admin topics: each is recognised (Matches true) and emits
// nothing without erroring. Their bodies are unmodelled (blocked on
// real on-chain samples), so they must not count as decode errors.
func TestDecode_vaultRebalanceAndAdminRecognisedEmit0Events(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	symbols := map[string]string{
		"rebalance": TopicSymbolRebalance,
		"rescue":    TopicSymbolRescue,
		"paused":    TopicSymbolPaused,
		"unpaused":  TopicSymbolUnpaused,
		"nreceiver": TopicSymbolNReceiver,
		"nmanager":  TopicSymbolNManager,
		"nemanager": TopicSymbolNEManager,
		"rbmanager": TopicSymbolRBManager,
		"dfees":     TopicSymbolDFees,
	}
	for name, sym := range symbols {
		t.Run(name, func(t *testing.T) {
			ev := events.Event{
				ContractID: MainnetVaults[0],
				Topic:      []string{TopicPrefixVault, sym},
			}
			if !d.Matches(ev) {
				t.Fatalf("Matches(vault %s) = false, want true", name)
			}
			out, err := d.Decode(ev)
			if err != nil {
				t.Errorf("Decode(vault %s) err = %v, want nil (recognised, unmodelled)", name, err)
			}
			if len(out) != 0 {
				t.Errorf("Decode(vault %s) emitted %d events, want 0", name, len(out))
			}
		})
	}
}

// TestDecodeRebalanceMethod exercises the four-way rebalance
// discriminator scaffolding (BACKLOG #58). It verifies the decoder
// reads the `rebalance_method` Symbol verbatim and that Known()
// classifies the four documented methods — WITHOUT asserting anything
// about the (unmodelled) per-method payload. Wire spelling for the
// four methods is unconfirmed on-chain; the decoder returns whatever
// the body carries, so a real sample can validate the exact values.
func TestDecodeRebalanceMethod(t *testing.T) {
	t.Parallel()

	documented := []RebalanceMethod{
		RebalanceUnwind, RebalanceInvest, RebalanceSwapExactIn, RebalanceSwapExactOut,
	}
	for _, want := range documented {
		t.Run("documented/"+string(want), func(t *testing.T) {
			ev := &events.Event{
				Topic: []string{TopicPrefixVault, TopicSymbolRebalance},
				Value: mustB64(t, mapSCVal(t,
					mapEntry(t, RebalanceMethodField, symSCVal(string(want))),
				)),
			}
			got, err := DecodeRebalanceMethod(ev)
			if err != nil {
				t.Fatalf("DecodeRebalanceMethod: %v", err)
			}
			if got != want {
				t.Errorf("method = %q, want %q", got, want)
			}
			if !got.Known() {
				t.Errorf("Known(%q) = false, want true", got)
			}
		})
	}

	t.Run("unknown method is read verbatim but not Known", func(t *testing.T) {
		ev := &events.Event{
			Value: mustB64(t, mapSCVal(t,
				mapEntry(t, RebalanceMethodField, symSCVal("some_future_method")),
			)),
		}
		got, err := DecodeRebalanceMethod(ev)
		if err != nil {
			t.Fatalf("DecodeRebalanceMethod: %v", err)
		}
		if got != RebalanceMethod("some_future_method") {
			t.Errorf("method = %q, want verbatim %q", got, "some_future_method")
		}
		if got.Known() {
			t.Errorf("Known(%q) = true, want false (unmodelled/renamed method)", got)
		}
	})

	t.Run("missing discriminator field is ErrMalformedPayload", func(t *testing.T) {
		ev := &events.Event{
			Value: mustB64(t, mapSCVal(t,
				mapEntry(t, "not_the_field", symSCVal("unwind")),
			)),
		}
		if _, err := DecodeRebalanceMethod(ev); !errors.Is(err, ErrMalformedPayload) {
			t.Errorf("err = %v, want ErrMalformedPayload", err)
		}
	})

	t.Run("discriminator not a Symbol is ErrMalformedPayload", func(t *testing.T) {
		ev := &events.Event{
			Value: mustB64(t, mapSCVal(t,
				mapEntry(t, RebalanceMethodField, i128SCVal(big.NewInt(7))),
			)),
		}
		if _, err := DecodeRebalanceMethod(ev); !errors.Is(err, ErrMalformedPayload) {
			t.Errorf("err = %v, want ErrMalformedPayload", err)
		}
	})
}
