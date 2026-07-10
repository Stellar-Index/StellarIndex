package chops

import (
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/StellarIndex/stellar-index/internal/dispatcher"
	"github.com/StellarIndex/stellar-index/internal/sources/classicmovements"
	"github.com/StellarIndex/stellar-index/internal/storage/clickhouse"
)

// TestAccountMovementsRealBytes_payment_fanOut chains a REAL pre-P23
// mainnet fixture (the exact bytes classicmovements/real_bytes_test.go's
// TestRealBytes_payment_success pins) through this command's
// accountMovementOf + clickhouse.FanOutAccountMovement, asserting the
// stellar.account_movements ROW SHAPE (not just the decode-time
// Movement) for a two-participant kind — one 'sent' row for the
// source, one 'received' row for the destination, correct
// counterparty on each. Lives in this package (not
// classicmovements' own real_bytes_test.go) because
// internal/sources/ may not import internal/storage/ (see
// scripts/ci/lint-imports.sh's L/sources-app-purity rule) — this is
// the one package that legitimately imports both.
func TestAccountMovementsRealBytes_payment_fanOut(t *testing.T) {
	// Same bytes as classicmovements' TestRealBytes_payment_success:
	// ledger 40000000, tx 0761af68c3cd6f5fc9a94b5ffca8129ccd6a3a6faa515fe1169706a8521ba248, op_index 2.
	const (
		bodyB64   = "AAAAAQAAAABs5He80fq3sKhGa7EvGdEJ9HUvB6qJt46lPuC0SkcnwwAAAAFYWEEAAAAAALh5cFAWaLQ6ZlreaIAMkEayxj7HzipA3vLbAaJXUi9wAAAAAo4EFBg="
		resultB64 = "AAAAAAAAAAEAAAAA"
		fromAddr  = "GA2QXW7YFAIR35LGKM2TDCQQZFR33XJCWF4N6SMRLOKX3HL76JKKPA62"
		toAddr    = "GBWOI5542H5LPMFIIZV3CLYZ2EE7I5JPA6VITN4OUU7OBNCKI4T4HMJP"
		asset     = "XXA-GC4HS4CQCZULIOTGLLPGRAAMSBDLFRR6Y7HCUQG66LNQDISXKIXXADIM"
		wantAmt   = "10972566552"
	)
	closeTime := time.Date(2022, 3, 12, 19, 32, 55, 0, time.UTC)

	m := decodeOneRealMovement(t, bodyB64, resultB64, 40_000_000,
		"0761af68c3cd6f5fc9a94b5ffca8129ccd6a3a6faa515fe1169706a8521ba248", 2, fromAddr, closeTime)
	if m.Kind != classicmovements.KindPayment {
		t.Fatalf("Kind = %q, want payment", m.Kind)
	}

	rows := clickhouse.FanOutAccountMovement(accountMovementOf(m))
	if len(rows) != 2 {
		t.Fatalf("FanOutAccountMovement produced %d rows, want 2 (two-participant payment)", len(rows))
	}

	var sent, received *clickhouse.AccountMovementRow
	for i := range rows {
		switch rows[i].Direction {
		case clickhouse.AccountMovementSent:
			sent = &rows[i]
		case clickhouse.AccountMovementReceived:
			received = &rows[i]
		case clickhouse.AccountMovementSelf:
			t.Fatalf("unexpected 'self' direction: %+v", rows[i])
		}
	}
	if sent == nil || received == nil {
		t.Fatalf("expected one sent + one received row, got %+v", rows)
	}
	if sent.Address != fromAddr || sent.Counterparty != toAddr {
		t.Errorf("sent row = address=%s counterparty=%s, want address=%s counterparty=%s",
			sent.Address, sent.Counterparty, fromAddr, toAddr)
	}
	if received.Address != toAddr || received.Counterparty != fromAddr {
		t.Errorf("received row = address=%s counterparty=%s, want address=%s counterparty=%s",
			received.Address, received.Counterparty, toAddr, fromAddr)
	}
	for _, r := range rows {
		if r.Asset != asset {
			t.Errorf("row asset = %q, want %q", r.Asset, asset)
		}
		if r.Amount == nil || r.Amount.String() != wantAmt {
			t.Errorf("row amount = %v, want %s", r.Amount, wantAmt)
		}
		if r.MovementKind != string(classicmovements.KindPayment) {
			t.Errorf("row movement_kind = %q, want payment", r.MovementKind)
		}
		if r.Ledger != 40_000_000 || r.TxHash != "0761af68c3cd6f5fc9a94b5ffca8129ccd6a3a6faa515fe1169706a8521ba248" || r.LegIndex != 0 {
			t.Errorf("row identity mismatch: %+v", r)
		}
	}
}

// TestAccountMovementsRealBytes_claimableBalanceCreate_fanOut chains a
// REAL pre-P23 mainnet CreateClaimableBalance fixture (the exact bytes
// classicmovements/real_bytes_test.go's
// TestRealBytes_createClaimableBalance_success pins) through the same
// accountMovementOf + FanOutAccountMovement path, asserting the
// single-participant "acting side" row shape (ADR-0048 D2): the
// creator, direction=sent, counterparty="" (no claimant known yet).
func TestAccountMovementsRealBytes_claimableBalanceCreate_fanOut(t *testing.T) {
	// Same bytes as classicmovements' TestRealBytes_createClaimableBalance_success:
	// ledger 40000000, tx 05fd6eea40b036204d6817d4ba945663e3071e2331385de78f38a9b9bed661cc, op_index 0.
	const (
		bodyB64   = "AAAADgAAAAFHQUxBAAAAADV6FeHCtmgz8V4u/kE1liwByrbw7SK8T5xatMG3j16DAAAAAAAF57gAAAACAAAAAAAAAACEKCUFXS4fGHiDDkqG2fsp03cR+Rd9bRJKybyy/rBgjgAAAAMAAAABAAAABAAAAABiLPP5AAAAAAAAAAA+CSgxOgl5fsc6se+nt2OaXHhgjaBK88g+wvrbZ7xZUgAAAAUAAAAAAAk6gA=="
		resultB64 = "AAAAAAAAAA4AAAAAAAAAAAZiRenTfqxyI9zYHNhROvZuqvaj0+hcn77xY+YUzgCd"
		creator   = "GCCCQJIFLUXB6GDYQMHEVBWZ7MU5G5YR7ELX23ISJLE3ZMX6WBQI4HFW"
		wantAsset = "GALA-GA2XUFPBYK3GQM7RLYXP4QJVSYWADSVW6DWSFPCPTRNLJQNXR5PIGALA"
		wantAmt   = "387000"
	)
	closeTime := time.Date(2022, 3, 12, 19, 32, 55, 0, time.UTC)

	m := decodeOneRealMovement(t, bodyB64, resultB64, 40_000_000,
		"05fd6eea40b036204d6817d4ba945663e3071e2331385de78f38a9b9bed661cc", 0, creator, closeTime)
	if m.Kind != classicmovements.KindClaimableBalanceCreate {
		t.Fatalf("Kind = %q, want claimable_balance_create", m.Kind)
	}

	rows := clickhouse.FanOutAccountMovement(accountMovementOf(m))
	if len(rows) != 1 {
		t.Fatalf("FanOutAccountMovement produced %d rows, want 1 (single-participant: no claimant known at creation)", len(rows))
	}
	row := rows[0]
	if row.Address != creator {
		t.Errorf("row.Address = %q, want %q (the creator)", row.Address, creator)
	}
	if row.Direction != clickhouse.AccountMovementSent {
		t.Errorf("row.Direction = %q, want sent", row.Direction)
	}
	if row.Counterparty != "" {
		t.Errorf("row.Counterparty = %q, want empty (no claimant resolved at creation time)", row.Counterparty)
	}
	if row.Asset != wantAsset || row.Amount == nil || row.Amount.String() != wantAmt {
		t.Errorf("row asset/amount = %s/%v, want %s/%s", row.Asset, row.Amount, wantAsset, wantAmt)
	}
	if row.MovementKind != string(classicmovements.KindClaimableBalanceCreate) {
		t.Errorf("row.MovementKind = %q, want claimable_balance_create", row.MovementKind)
	}
}

// decodeOneRealMovement decodes one real mainnet op/result pair via the
// production classicmovements.Decoder — the same call
// classic-movements-backfill's main loop makes against
// clickhouse.StreamClassicOps output — and returns its single decoded
// Movement. Mirrors classicmovements' own (unexported, package-local)
// decodeRealBytes test helper; duplicated here rather than shared
// because internal/sources/ test helpers aren't importable across
// packages and this package is the one place both classicmovements and
// clickhouse can be imported together (see this file's package doc).
func decodeOneRealMovement(t *testing.T, bodyB64, resultB64 string, ledger uint32, txHash string, opIndex uint32, fromAddr string, closedAt time.Time) classicmovements.Movement {
	t.Helper()
	var body xdr.OperationBody
	if err := xdr.SafeUnmarshalBase64(bodyB64, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	var result xdr.OperationResult
	if err := xdr.SafeUnmarshalBase64(resultB64, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	op := xdr.Operation{Body: body}

	outs, err := classicmovements.NewDecoder().Decode(dispatcher.OpContext{
		Ledger:   ledger,
		ClosedAt: closedAt,
		TxHash:   txHash,
		TxSource: fromAddr,
		OpIndex:  int(opIndex),
		Op:       op,
		OpResult: result,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(outs) != 1 {
		t.Fatalf("Decode produced %d outputs, want 1", len(outs))
	}
	me, ok := outs[0].(classicmovements.MovementEvent)
	if !ok {
		t.Fatalf("output is %T, want classicmovements.MovementEvent", outs[0])
	}
	return me.Movement
}
