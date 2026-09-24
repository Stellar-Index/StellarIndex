package sorocredit

import (
	"encoding/base64"
	"errors"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// ─── real-lake golden frames (base64 SCVal) ──────────────────────────
//
// Captured verbatim from real mainnet ledgers emitted by the main
// contract (MainnetContract) on the r1 ClickHouse lake, 2026-07-07.
// These PIN the decoded schemas: if a decode helper drifts, the asserted
// promoted fields change. topics_xdr / data_xdr are the raw event bytes.

var goldenFrames = map[string]struct {
	topics []string
	data   string
}{
	// topics=[Symbol, String(statement_uuid), String(position_uuid)];
	// data=Vec[i128 amount, Address collateral, u64 timestamp].
	"StatementPublished": {
		topics: []string{
			"AAAADwAAABJTdGF0ZW1lbnRQdWJsaXNoZWQAAA==",
			"AAAADgAAACQ4MWQ5Zjc5Yy1kZWU3LTRjNWUtYmRlMS02YTc3OTk2MGU4N2Q=",
			"AAAADgAAACRiZmM5N2JhYy0zYjg1LTRkNzAtOWE1My0yYzQ0NzViZjMwZGM=",
		},
		data: "AAAAEAAAAAEAAAADAAAACgAAAAAAAAAAAAAAAAAACTgAAAASAAAAAfUFwkKWgmvUumh0WWb3knRxPR7Pf0EWTh52PHDjsHYLAAAABQAAAABqS8Fm",
	},
	// topics=[Symbol, Address(collateral child), String(position_uuid),
	// String(statement_uuid)]; data=Vec[Address settler, Vec[Address]
	// debt_assets, Vec[i128] amounts, …trailing protocol-internal fields].
	"Liquidation": {
		topics: []string{
			"AAAADwAAAAtMaXF1aWRhdGlvbgA=",
			"AAAAEgAAAAHrsxqxsdNNsWaVyHEsDHPmhfrJhZmbzlk7znjvFPOWgA==",
			"AAAADgAAACRkNWM2YmM3OS0yNzQyLTRjZGQtOTAxOC05NDcxMTVmNzdiMDg=",
			"AAAADgAAACRmZTg2ZDIyZC01OTJmLTRjMTQtYjRiNy04ZWYzNjNlYzE2MzI=",
		},
		data: "AAAAEAAAAAEAAAAHAAAAEgAAAAAAAAAANvtfZ+gQcaLJxiWzd8cLzDwQARYPyziiud12Pp7XE+MAAAAQAAAAAQAAAAEAAAASAAAAAa3vzlmu5Slo92Bh1JTCUlt1ZZ+kKWpl9JnvKeVkd+SWAAAAEAAAAAEAAAABAAAACgAAAAAAAAAAAAAAAAy4OcAAAAAQAAAAAQAAAAEAAAAKAAAAAAAAAAAAAAAAAAAIVgAAABAAAAABAAAAAQAAAAMAAAAHAAAACgAAAAAAAAAAAAAAAAAACFYAAAAQAAAAAQAAAAEAAAAKAAAAAAAAAAAAAAAAAAAAAA==",
	},
	// topics=[Symbol, Address(child)]; data=Vec[String name, Address owner].
	"NewCollateralContract": {
		topics: []string{
			"AAAADwAAABVOZXdDb2xsYXRlcmFsQ29udHJhY3QAAAA=",
			"AAAAEgAAAAFvwu+BE6690V76q9574JuQfy8McX+YXK2gl/mCZ70Xpw==",
		},
		data: "AAAAEAAAAAEAAAACAAAADgAAAC9Db2xsYXRlcmFsLTAzODU1MzRhLTczY2EtNDIyZi1iNDQ5LTU5YTExOTZhYWNiNgAAAAASAAAAAAAAAAB4poQ4eoY+oU3UIUJVTMJaJFQwlykKA/LmJa9ILTqSaQ==",
	},
	// topics=[Symbol, Address(collateral)]; data=Vec[Address token,
	// Address recipient, i128 amount].
	"Withdrawal": {
		topics: []string{
			"AAAADwAAAApXaXRoZHJhd2FsAAA=",
			"AAAAEgAAAAHqql4dye1i8MrrJdWp+wBbalJqsfx3zBBtt+d/WfVFsw==",
		},
		data: "AAAAEAAAAAEAAAADAAAAEgAAAAGt785ZruUpaPdgYdSUwlJbdWWfpClqZfSZ7ynlZHfklgAAABIAAAAAAAAAAIbxw2DJ/zT7LB/y7us78QoFnH2oRpR8ZWw85zd37rm3AAAACgAAAAAAAAAAAAAAAAFrKMA=",
	},
	// topics=[Symbol]; data=Vec[Void old, Address new].
	"BeaconUpdated": {
		topics: []string{"AAAADwAAAA1CZWFjb25VcGRhdGVkAAAA"},
		data:   "AAAAEAAAAAEAAAACAAAAAQAAABIAAAABsgQFpBbjMfH+QgBpCeRCp2/i7UW2NoQrX2PWHRxlUQA=",
	},
	// topics=[Symbol, Address(asset)]; data=Vec[…config…].
	"SupportedAssetAdded": {
		topics: []string{
			"AAAADwAAABNTdXBwb3J0ZWRBc3NldEFkZGVkAA==",
			"AAAAEgAAAAGt785ZruUpaPdgYdSUwlJbdWWfpClqZfSZ7ynlZHfklg==",
		},
		data: "AAAAEAAAAAEAAAAHAAAAAQAAAAEAAAABAAAAAQAAAAEAAAAKAAAAAAAAAAAAAAAAAAAAAAAAAAoAAAAAAAAAAAAAAAAAAAAA",
	},
	// topics=[Symbol]; data=Vec[Bytes old_hash, Bytes new_hash].
	"CollateralHashUpdated": {
		topics: []string{"AAAADwAAABVDb2xsYXRlcmFsSGFzaFVwZGF0ZWQAAAA="},
		data:   "AAAAEAAAAAEAAAACAAAADQAAACAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA0AAAAgBwl/g9rjt0bbfboyY9nMM077iKmn1UUPuWyhnzPShLA=",
	},
	// topics=[Symbol]; data=Vec[Address old_treasury, Address new_treasury].
	// The recognition-audit gap: 1 real event, main contract, ledger
	// 63,847,367 (tx 836dab51…). Config event captured verbatim.
	"TreasuryUpdated": {
		topics: []string{"AAAADwAAAA9UcmVhc3VyeVVwZGF0ZWQA"},
		data:   "AAAAEAAAAAEAAAACAAAAEgAAAAAAAAAAEZtPs71BPlSCnqFeq9TIFCYNJTo5SboNEpuEDGoNoS8AAAASAAAAAAAAAADtsFSvh+1HUA2gH0Yq8MVwvPFIGYhHKe0lT+mBd36mQg==",
	},
}

func goldenEvent(t *testing.T, name string) events.Event {
	t.Helper()
	f, ok := goldenFrames[name]
	if !ok {
		t.Fatalf("no golden frame %q", name)
	}
	return events.Event{
		Type:           "contract",
		ContractID:     MainnetContract,
		Ledger:         63_356_218,
		LedgerClosedAt: "2026-07-06T15:25:16Z",
		TxHash:         "6714f83ef3f94a76f0158ff4ee76a6a452cb5677cf6c10024583b2b5974ccf8a",
		Topic:          f.topics,
		Value:          f.data,
	}
}

func mustDecodeOne(t *testing.T, name string) Event {
	t.Helper()
	ev := goldenEvent(t, name)
	out, err := decodeOne(&ev)
	if err != nil {
		t.Fatalf("decodeOne(%s): %v", name, err)
	}
	return out
}

// TestGolden_StatementPublished pins the statement decode (i128 amount,
// address collateral, u64 timestamp; UUIDs from topics).
func TestGolden_StatementPublished(t *testing.T) {
	t.Parallel()
	e := mustDecodeOne(t, "StatementPublished")
	if e.EventType != TypeStatement {
		t.Fatalf("EventType = %q, want %q", e.EventType, TypeStatement)
	}
	if e.StatementUUID != "81d9f79c-dee7-4c5e-bde1-6a779960e87d" {
		t.Errorf("StatementUUID = %q", e.StatementUUID)
	}
	if e.PositionUUID != "bfc97bac-3b85-4d70-9a53-2c4475bf30dc" {
		t.Errorf("PositionUUID = %q", e.PositionUUID)
	}
	if e.Amount != "2360" {
		t.Errorf("Amount = %q, want 2360", e.Amount)
	}
	if e.CollateralContract == "" || e.CollateralContract[0] != 'C' {
		t.Errorf("CollateralContract = %q, want a contract strkey", e.CollateralContract)
	}
	if e.StatementTime == nil || e.StatementTime.Unix() != 1783349606 {
		t.Errorf("StatementTime = %v, want unix 1783349606", e.StatementTime)
	}
}

// TestGolden_Settlement pins the "Liquidation"→settlement decode and
// asserts the SCHEDULED-SETTLEMENT naming (EventType/kind is `settlement`,
// NEVER `liquidation`).
func TestGolden_Settlement(t *testing.T) {
	t.Parallel()
	e := mustDecodeOne(t, "Liquidation")
	if e.EventType != TypeSettlement {
		t.Fatalf("EventType = %q, want %q (scheduled settlement, NOT liquidation)", e.EventType, TypeSettlement)
	}
	if e.EventKind() != "sorocredit.settlement" {
		t.Errorf("EventKind() = %q, want sorocredit.settlement — must NOT surface as a liquidation", e.EventKind())
	}
	if e.PositionUUID != "d5c6bc79-2742-4cdd-9018-947115f77b08" {
		t.Errorf("PositionUUID = %q", e.PositionUUID)
	}
	if e.StatementUUID != "fe86d22d-592f-4c14-b4b7-8ef363ec1632" {
		t.Errorf("StatementUUID = %q", e.StatementUUID)
	}
	if e.Account == "" || e.Account[0] != 'G' {
		t.Errorf("Account (settler) = %q, want an account strkey", e.Account)
	}
	if e.Asset == "" || e.Asset[0] != 'C' {
		t.Errorf("Asset (debt asset) = %q, want a contract strkey", e.Asset)
	}
	if e.Amount != "213400000" {
		t.Errorf("Amount (settled) = %q, want 213400000", e.Amount)
	}
	if _, ok := e.Attributes["body"]; !ok {
		t.Errorf("settlement should stash the full body in Attributes[\"body\"]")
	}
}

// TestGolden_NewCollateralContract pins the position-open decode: child
// C-address in topic[1], name/owner in the body, UUID parsed from name.
func TestGolden_NewCollateralContract(t *testing.T) {
	t.Parallel()
	e := mustDecodeOne(t, "NewCollateralContract")
	if e.EventType != TypeNewCollateralContract {
		t.Fatalf("EventType = %q", e.EventType)
	}
	if e.CollateralContract == "" || e.CollateralContract[0] != 'C' {
		t.Errorf("CollateralContract (child) = %q, want a contract strkey", e.CollateralContract)
	}
	if e.PositionName != "Collateral-0385534a-73ca-422f-b449-59a1196aacb6" {
		t.Errorf("PositionName = %q", e.PositionName)
	}
	if e.PositionUUID != "0385534a-73ca-422f-b449-59a1196aacb6" {
		t.Errorf("PositionUUID = %q (should strip the Collateral- prefix)", e.PositionUUID)
	}
	if e.Owner == "" || e.Owner[0] != 'G' {
		t.Errorf("Owner = %q, want an account strkey", e.Owner)
	}
}

// TestDecode_NewCollateralContract_MissingPrefixRejected proves Q078's
// fix: TrimPrefix silently returns the whole string when the "Collateral-"
// prefix is absent, which used to mint a PositionUUID that could never
// match a real StatementPublished/Liquidation topic — a permanent join
// miss disguised as "no statement yet". A body name lacking the prefix
// must now fail decode instead of fabricating a bogus UUID.
func TestDecode_NewCollateralContract_MissingPrefixRejected(t *testing.T) {
	t.Parallel()
	child := contractStrkey(t, 0x02)
	body := b64(t, vecSV(
		stringSV("not-the-expected-shape"),
		contractAddrSV(t, contractStrkey(t, 0x03)),
	))
	ev := events.Event{
		LedgerClosedAt: "2026-07-06T00:00:00Z",
		Topic: []string{
			topicSymNewCollateralContract,
			b64(t, contractAddrSV(t, child)),
		},
		Value: body,
	}
	_, err := decodeOne(&ev)
	if !errors.Is(err, ErrMalformedPayload) {
		t.Fatalf("decodeOne: want ErrMalformedPayload, got %v", err)
	}
}

// TestGolden_Withdrawal pins the withdrawal decode (token, recipient,
// i128 amount from the body; collateral from the topic).
func TestGolden_Withdrawal(t *testing.T) {
	t.Parallel()
	e := mustDecodeOne(t, "Withdrawal")
	if e.EventType != TypeWithdrawal {
		t.Fatalf("EventType = %q", e.EventType)
	}
	if e.CollateralContract == "" || e.CollateralContract[0] != 'C' {
		t.Errorf("CollateralContract = %q", e.CollateralContract)
	}
	if e.Asset == "" || e.Asset[0] != 'C' {
		t.Errorf("Asset (token) = %q", e.Asset)
	}
	if e.Account == "" || e.Account[0] != 'G' {
		t.Errorf("Account (recipient) = %q", e.Account)
	}
	if e.Amount != "23800000" {
		t.Errorf("Amount = %q, want 23800000", e.Amount)
	}
}

// TestGolden_ConfigEvents decodes the three low-volume config events and
// asserts they route to credit_events (Withdrawal + config) with the body
// captured — the EVERY-event invariant (no silent drop).
func TestGolden_ConfigEvents(t *testing.T) {
	t.Parallel()
	cases := []struct {
		frame string
		typ   EventType
	}{
		{"BeaconUpdated", TypeBeaconUpdated},
		{"SupportedAssetAdded", TypeSupportedAssetAdded},
		{"CollateralHashUpdated", TypeCollateralHashUpdated},
		{"TreasuryUpdated", TypeTreasuryUpdated},
	}
	for _, c := range cases {
		c := c
		t.Run(c.frame, func(t *testing.T) {
			t.Parallel()
			e := mustDecodeOne(t, c.frame)
			if e.EventType != c.typ {
				t.Fatalf("EventType = %q, want %q", e.EventType, c.typ)
			}
			if _, ok := e.Attributes["body"]; !ok {
				t.Errorf("%s should capture the body in Attributes[\"body\"]", c.frame)
			}
		})
	}
	// SupportedAssetAdded promotes the asset from topic[1].
	e := mustDecodeOne(t, "SupportedAssetAdded")
	if e.Asset == "" || e.Asset[0] != 'C' {
		t.Errorf("SupportedAssetAdded Asset = %q, want a contract strkey", e.Asset)
	}
}

// TestGolden_BodyCapturedVerbatim pins the "nothing is dropped" / "captured
// verbatim" promise made by decodeSettlement, decodeSupportedAssetAdded and
// decodeConfigBody's godocs: Attributes["body"] must equal the event's raw
// base64 XDR payload byte-for-byte, not a rendering of it. scval.Display is
// lossy by design (120-rune truncation, depth-3 cap, exotic types degrade
// to their type name) — running the audit-trail capture through it would
// silently violate the promise for any body that trips those limits.
func TestGolden_BodyCapturedVerbatim(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"Liquidation", "SupportedAssetAdded", "BeaconUpdated",
		"CollateralHashUpdated", "TreasuryUpdated",
	} {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := mustDecodeOne(t, name)
			got, ok := e.Attributes["body"]
			if !ok {
				t.Fatalf("%s: Attributes[%q] missing", name, "body")
			}
			want := goldenFrames[name].data
			if got != want {
				t.Errorf("%s: Attributes[\"body\"] = %q, want the raw base64 body %q verbatim", name, got, want)
			}
		})
	}
}

// TestGolden_RoundTripEveryFrame runs the full classify→decode→Event join
// over every golden frame (all 8 event types decode without error, carry
// the parsed timestamp, and report the right source).
func TestGolden_RoundTripEveryFrame(t *testing.T) {
	t.Parallel()
	for name := range goldenFrames {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := mustDecodeOne(t, name)
			if e.ObservedAt.IsZero() {
				t.Error("ObservedAt should be parsed from LedgerClosedAt")
			}
			if e.Source() != SourceName {
				t.Errorf("Source() = %q, want %q", e.Source(), SourceName)
			}
		})
	}
}

// ─── Classify ────────────────────────────────────────────────────────

func TestClassify_AllEightSymbols(t *testing.T) {
	t.Parallel()
	cases := []struct {
		topic string
		want  EventType
	}{
		{topicSymNewCollateralContract, TypeNewCollateralContract},
		{topicSymStatementPublished, TypeStatement},
		{topicSymLiquidation, TypeSettlement},
		{topicSymWithdrawal, TypeWithdrawal},
		{topicSymBeaconUpdated, TypeBeaconUpdated},
		{topicSymSupportedAssetAdded, TypeSupportedAssetAdded},
		{topicSymCollateralHashUpdated, TypeCollateralHashUpdated},
		{topicSymTreasuryUpdated, TypeTreasuryUpdated},
	}
	for _, c := range cases {
		c := c
		t.Run(string(c.want), func(t *testing.T) {
			t.Parallel()
			if got := classify(&events.Event{Topic: []string{c.topic}}); got != c.want {
				t.Errorf("classify = %q, want %q", got, c.want)
			}
		})
	}
}

func TestClassify_UnknownAndEmpty(t *testing.T) {
	t.Parallel()
	if got := classify(&events.Event{Topic: []string{b64Symbol(t, "transfer")}}); got != "" {
		t.Errorf("unknown topic classified as %q", got)
	}
	if got := classify(&events.Event{Topic: nil}); got != "" {
		t.Errorf("empty topic classified as %q", got)
	}
}

// TestEventSymbols_MatchesClassify guards the historical failure mode
// that dropped TreasuryUpdated recognition under ADR-0033: EventSymbols()
// and classify() were two independently hand-written lists of the same
// topics, and one could gain an entry the other lacked. This test is
// driven off EventSymbols() itself (not a second hardcoded list), so a
// topic present there but unrecognized by classify() fails loudly.
func TestEventSymbols_MatchesClassify(t *testing.T) {
	t.Parallel()
	syms := EventSymbols()
	if len(syms) != 8 {
		t.Fatalf("EventSymbols() returned %d symbols, want 8", len(syms))
	}
	seen := make(map[EventType]bool, len(syms))
	for _, topic := range syms {
		enc := scval.MustEncodeSymbol(topic)
		got := classify(&events.Event{Topic: []string{enc}})
		if got == "" {
			t.Errorf("classify() did not recognize EventSymbols() topic %q", topic)
			continue
		}
		if seen[got] {
			t.Errorf("EventType %q claimed by more than one EventSymbols() topic", got)
		}
		seen[got] = true
	}
	if len(seen) != len(syms) {
		t.Errorf("classify() recognized %d distinct EventTypes from %d EventSymbols() topics, want them equal", len(seen), len(syms))
	}
}

// ─── gating (ADR-0035) ───────────────────────────────────────────────

// TestMatches_TrustRootAcceptedLookAlikeRejected is the gate test: every
// tracked symbol from the trust-root main contract matches; the SAME
// symbol from a foreign contract (the two real look-alike emitters) does
// NOT — identity gating, not topic matching.
func TestMatches_TrustRootAcceptedLookAlikeRejected(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	lookAlike := contractStrkey(t, 0xEE)
	for name := range goldenFrames {
		ev := goldenEvent(t, name)
		if !d.Matches(ev) {
			t.Errorf("%s from the trust root should MATCH", name)
		}
		ev.ContractID = lookAlike
		if d.Matches(ev) {
			t.Errorf("%s from a look-alike contract must be REJECTED (identity gate)", name)
		}
	}
}

// TestMatches_UnknownSymbolRejected — a foreign topic from the trust root
// is not claimed.
func TestMatches_UnknownSymbolRejected(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	ev := events.Event{ContractID: MainnetContract, Topic: []string{b64Symbol(t, "swap")}}
	if d.Matches(ev) {
		t.Error("a non-sorocredit topic must not match even from the trust root")
	}
}

// TestChildgate_SeedThenAccept exercises the forward-compat childgate:
// decoding a NewCollateralContract seeds the announced child, after which
// a (hypothetical) business event emitted BY that child is accepted —
// while NewCollateralContract itself stays trust-root-only.
func TestChildgate_SeedThenAccept(t *testing.T) {
	t.Parallel()
	d := NewDecoder()

	// A child event before any seed is rejected (unknown contract).
	childEv := goldenEvent(t, "Withdrawal")
	// The real child C-address announced by the golden NewCollateralContract.
	newColl := goldenEvent(t, "NewCollateralContract")
	out, err := d.Decode(newColl)
	if err != nil {
		t.Fatalf("Decode(NewCollateralContract): %v", err)
	}
	child := out[0].(Event).CollateralContract
	if child == "" {
		t.Fatal("expected a decoded child collateral contract")
	}

	// A Withdrawal emitted BY that seeded child now passes the gate.
	childEv.ContractID = child
	if !d.Matches(childEv) {
		t.Error("a business event from a seeded child should match (childgate forward-compat)")
	}

	// NewCollateralContract from the child (not the trust root) is rejected.
	newColl.ContractID = child
	if d.Matches(newColl) {
		t.Error("NewCollateralContract must be trust-root-only — a child cannot announce a position")
	}
}

// ─── ADR-0003 large-i128 guard ───────────────────────────────────────

// TestDecode_LargeI128_NoTruncation feeds a statement amount well above
// 2^53 and asserts the full-precision decimal string survives.
func TestDecode_LargeI128_NoTruncation(t *testing.T) {
	t.Parallel()
	big1, _ := new(big.Int).SetString("170141183460469231731687303715884105727", 10) // 2^127-1
	body := b64(t, vecSV(
		i128SV(big1),
		contractAddrSV(t, contractStrkey(t, 0x01)),
		u64SV(1_700_000_000),
	))
	ev := events.Event{
		LedgerClosedAt: "2026-07-06T00:00:00Z",
		Topic: []string{
			topicSymStatementPublished,
			b64(t, stringSV("stmt")),
			b64(t, stringSV("pos")),
		},
		Value: body,
	}
	out, err := decodeOne(&ev)
	if err != nil {
		t.Fatalf("decodeOne: %v", err)
	}
	if out.Amount != big1.String() {
		t.Errorf("large i128 lost precision: got %q, want %q", out.Amount, big1.String())
	}
}

// ─── Q074: NUL/invalid-UTF8 UUID and u64 epoch overflow ──────────────

// TestDecode_StatementPublished_NULUUIDSurvivesAsHexText proves Q074's
// poison-pill fix: a statement_uuid/position_uuid is bytes chosen by
// whoever calls the contract, bound to a `text NOT NULL` column. Before
// the fix, strTopic decoded it through scval.AsString and bound the raw
// bytes unchecked — a NUL or invalid-UTF8 value there would be refused by
// Postgres (SQLSTATE 22021) and the row lost for good. It must now come
// back as scval.ToText's lossless hex form, never the raw NUL-bearing
// string.
func TestDecode_StatementPublished_NULUUIDSurvivesAsHexText(t *testing.T) {
	t.Parallel()
	rawUUID := "bad\x00uuid-not-utf8-\xff"
	body := b64(t, vecSV(
		i128SV(big.NewInt(2360)),
		contractAddrSV(t, contractStrkey(t, 0x01)),
		u64SV(1_700_000_000),
	))
	ev := events.Event{
		LedgerClosedAt: "2026-07-06T00:00:00Z",
		Topic: []string{
			topicSymStatementPublished,
			b64(t, stringSV("stmt-ok")),
			b64(t, stringSV(rawUUID)),
		},
		Value: body,
	}
	out, err := decodeOne(&ev)
	if err != nil {
		t.Fatalf("decodeOne: %v", err)
	}
	if strings.Contains(out.PositionUUID, "\x00") {
		t.Fatalf("PositionUUID still carries a raw NUL byte: %q — would hit SQLSTATE 22021 on insert", out.PositionUUID)
	}
	want := scval.ToText(rawUUID)
	if out.PositionUUID != want {
		t.Errorf("PositionUUID = %q, want the scval.ToText hex form %q", out.PositionUUID, want)
	}
}

// TestDecode_StatementPublished_TimestampOverflowFallsBackToClosedAt
// proves Q074's other half: decodeStatement cast the on-wire u64
// timestamp straight to int64 with a //nolint:gosec silencing the
// overflow check. A value above math.MaxInt64 wraps negative under that
// cast (the same overflow class as the router deadline_ts bug) and would
// stamp a bogus 1969 StatementTime. It must instead fall back to the
// ledger close time.
func TestDecode_StatementPublished_TimestampOverflowFallsBackToClosedAt(t *testing.T) {
	t.Parallel()
	closedAtRFC := "2026-07-06T00:00:00Z"
	body := b64(t, vecSV(
		i128SV(big.NewInt(2360)),
		contractAddrSV(t, contractStrkey(t, 0x01)),
		u64SV(math.MaxUint64),
	))
	ev := events.Event{
		LedgerClosedAt: closedAtRFC,
		Topic: []string{
			topicSymStatementPublished,
			b64(t, stringSV("stmt")),
			b64(t, stringSV("pos")),
		},
		Value: body,
	}
	out, err := decodeOne(&ev)
	if err != nil {
		t.Fatalf("decodeOne: %v", err)
	}
	wantClosedAt, perr := time.Parse(time.RFC3339, closedAtRFC)
	if perr != nil {
		t.Fatalf("time.Parse: %v", perr)
	}
	if out.StatementTime == nil {
		t.Fatal("StatementTime is nil")
	}
	if !out.StatementTime.Equal(wantClosedAt) {
		t.Errorf("StatementTime = %v, want the ledger-close fallback %v (an unchecked cast wraps negative instead)", out.StatementTime, wantClosedAt)
	}
}

// ─── malformed-event guards ──────────────────────────────────────────

func TestDecode_ShortTopic(t *testing.T) {
	t.Parallel()
	ev := events.Event{LedgerClosedAt: "2026-07-06T00:00:00Z", Topic: []string{topicSymStatementPublished}}
	if _, err := decodeOne(&ev); !errors.Is(err, ErrMalformedPayload) {
		t.Errorf("statement short-topic: want ErrMalformedPayload, got %v", err)
	}
}

func TestDecode_UntrackedTopic(t *testing.T) {
	t.Parallel()
	ev := events.Event{LedgerClosedAt: "2026-07-06T00:00:00Z", Topic: []string{b64Symbol(t, "transfer")}}
	if _, err := decodeOne(&ev); !errors.Is(err, ErrNotSoroCreditEvent) {
		t.Errorf("untracked topic: want ErrNotSoroCreditEvent, got %v", err)
	}
}

// ─── synthetic SCVal helpers (xdr confined to the test file) ─────────

func stringSV(s string) xdr.ScVal {
	x := xdr.ScString(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &x}
}

func symbolSV(s string) xdr.ScVal {
	sym := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

func i128SV(n *big.Int) xdr.ScVal {
	hi, lo := split128(n)
	p := xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &p}
}

func u64SV(v uint64) xdr.ScVal {
	x := xdr.Uint64(v)
	return xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &x}
}

func vecSV(vals ...xdr.ScVal) xdr.ScVal {
	v := xdr.ScVec(vals)
	pv := &v
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pv}
}

func contractAddrSV(t *testing.T, strk string) xdr.ScVal {
	t.Helper()
	var cid xdr.ContractId
	raw, err := strkey.Decode(strkey.VersionByteContract, strk)
	if err != nil {
		t.Fatalf("strkey.Decode: %v", err)
	}
	copy(cid[:], raw)
	a := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &a}
}

func contractStrkey(t *testing.T, seed byte) string {
	t.Helper()
	var raw [32]byte
	raw[0] = seed
	s, err := strkey.Encode(strkey.VersionByteContract, raw[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	return s
}

func b64(t *testing.T, sv xdr.ScVal) string {
	t.Helper()
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func b64Symbol(t *testing.T, s string) string {
	t.Helper()
	return b64(t, symbolSV(s))
}

// TestDecode_Settlement_MalformedAmountCountsDegrade proves Q072's fix:
// a Liquidation body whose amounts leg (data[2]) isn't a non-empty
// Vec[i128] must still decode (nil error, row kept — see package doc)
// but now increments obs.SourceAmountDegradedTotal{source="sorocredit",
// field="settled_amount"} so the resulting NULL settled_amount is no
// longer invisible.
func TestDecode_Settlement_MalformedAmountCountsDegrade(t *testing.T) {
	before := testutil.ToFloat64(obs.SourceAmountDegradedTotal.WithLabelValues(SourceName, "settled_amount"))

	ev := settlementEvent(t,
		vecSV(contractAddrSV(t, contractStrkey(t, 0x02))), // debt_assets (ok)
		vecSV(), // amounts — empty Vec, the malformed shape
		4,
	)

	out, err := decodeOne(&ev)
	if err != nil {
		t.Fatalf("decodeOne: %v", err)
	}
	if out.Amount != "" {
		t.Fatalf("Amount = %q, want empty (malformed amounts leg)", out.Amount)
	}

	after := testutil.ToFloat64(obs.SourceAmountDegradedTotal.WithLabelValues(SourceName, "settled_amount"))
	if after != before+1 {
		t.Errorf("SourceAmountDegradedTotal{source=%q,field=%q} = %v, want %v (the degrade must be counted, not just noted in attrs)", SourceName, "settled_amount", after, before+1)
	}
}

func split128(n *big.Int) (hi int64, lo uint64) {
	twoTo64 := new(big.Int).Lsh(big.NewInt(1), 64)
	mask64 := new(big.Int).Sub(twoTo64, big.NewInt(1))
	if n.Sign() >= 0 {
		loBig := new(big.Int).And(n, mask64)
		hiBig := new(big.Int).Rsh(n, 64)
		return hiBig.Int64(), loBig.Uint64()
	}
	twoTo128 := new(big.Int).Lsh(big.NewInt(1), 128)
	u := new(big.Int).Add(twoTo128, n)
	loBig := new(big.Int).And(u, mask64)
	hiBig := new(big.Int).Rsh(u, 64)
	return int64(hiBig.Uint64()), loBig.Uint64()
}

// settlementEvent builds a Liquidation event whose body is
// Vec[settler, assets, amounts, <trailing filler elements>].
func settlementEvent(t *testing.T, assets, amounts xdr.ScVal, trailing int) events.Event {
	t.Helper()
	elems := []xdr.ScVal{contractAddrSV(t, contractStrkey(t, 0x01)), assets, amounts}
	for range trailing {
		elems = append(elems, vecSV())
	}
	return events.Event{
		LedgerClosedAt: "2026-07-06T00:00:00Z",
		Topic: []string{
			topicSymLiquidation,
			b64(t, contractAddrSV(t, contractStrkey(t, 0x03))),
			b64(t, stringSV("pos")),
			b64(t, stringSV("stmt")),
		},
		Value: b64(t, vecSV(elems...)),
	}
}

// TestDecode_Settlement_BodyMustBeAuditedSevenVec pins the audited
// Liquidation body arity (wasm-audits/sorocredit.md: Vec[7]). Any other
// length is an unaudited shape whose positional promotion is untrusted,
// so it must fail as ErrMalformedPayload rather than promote data[0..2].
func TestDecode_Settlement_BodyMustBeAuditedSevenVec(t *testing.T) {
	t.Parallel()
	debtAsset := contractStrkey(t, 0x02)
	assets := vecSV(contractAddrSV(t, debtAsset))
	amounts := vecSV(i128SV(big.NewInt(213400000)))
	for _, trailing := range []int{0, 3, 5} {
		ev := settlementEvent(t, assets, amounts, trailing)
		out, err := decodeOne(&ev)
		if !errors.Is(err, ErrMalformedPayload) {
			t.Errorf("body len %d: err = %v, want ErrMalformedPayload (promoted Asset=%q Amount=%q)",
				3+trailing, err, out.Asset, out.Amount)
		}
	}
	ev := settlementEvent(t, assets, amounts, 4)
	out, err := decodeOne(&ev)
	if err != nil {
		t.Fatalf("audited 7-Vec body: decodeOne: %v", err)
	}
	if out.Asset != debtAsset || out.Amount != "213400000" {
		t.Errorf("audited 7-Vec body: Asset=%q Amount=%q, want %q / 213400000", out.Asset, out.Amount, debtAsset)
	}
}

// TestDecode_Settlement_NonParallelLegsNotPromoted: debt_assets and
// amounts pair by index, so when their lengths differ amounts[0] is not
// provably assets[0]'s amount. Neither may be promoted; the degrade is
// noted in attributes and counted.
func TestDecode_Settlement_NonParallelLegsNotPromoted(t *testing.T) {
	before := testutil.ToFloat64(obs.SourceAmountDegradedTotal.WithLabelValues(SourceName, "settled_amount"))
	assets := vecSV(contractAddrSV(t, contractStrkey(t, 0x02)), contractAddrSV(t, contractStrkey(t, 0x04)))
	amounts := vecSV(i128SV(big.NewInt(213400000)))
	ev := settlementEvent(t, assets, amounts, 4)

	out, err := decodeOne(&ev)
	if err != nil {
		t.Fatalf("decodeOne: %v", err)
	}
	if out.Asset != "" || out.Amount != "" {
		t.Errorf("non-parallel legs promoted Asset=%q Amount=%q, want both empty", out.Asset, out.Amount)
	}
	for _, k := range []string{"debt_asset_error", "settled_amount_error"} {
		if _, ok := out.Attributes[k]; !ok {
			t.Errorf("Attributes missing %q: %v", k, out.Attributes)
		}
	}
	after := testutil.ToFloat64(obs.SourceAmountDegradedTotal.WithLabelValues(SourceName, "settled_amount"))
	if after != before+1 {
		t.Errorf("SourceAmountDegradedTotal settled_amount = %v, want %v", after, before+1)
	}
}
