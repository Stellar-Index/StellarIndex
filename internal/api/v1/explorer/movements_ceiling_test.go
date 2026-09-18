// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package explorer

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// These tests pin F055 on GET /v1/accounts/{g}/movements: the cap67
// ceiling that separates the ClickHouse archive arm from the Postgres
// tail must be a PREDICATE ON THE CLICKHOUSE READ, never a trim of the
// page the read already returned.
//
// The un-fixed handler asked ClickHouse for the `limit` newest rows and
// then dropped every row above the ceiling in Go. While the cap67 derive
// is mid-window — and permanently for any account that moves more than a
// page's worth of value per derive tick — all `limit` fetched rows sit
// above the ceiling, so the page collapsed to zero rows; `len(merged) ==
// limit` is then false, next_cursor is suppressed, and every movement
// BELOW the ceiling (the account's whole pre-watermark history) became
// unreachable through this endpoint.

// TestAccountMovements_CeilingIsAppliedBeforeTheLimit is the F055
// regression: with a cap67 watermark below the account's newest rows, a
// first page must come back FULL of rows at-or-below the watermark and
// carry a next_cursor, instead of being emptied by a post-read trim.
func TestAccountMovements_CeilingIsAppliedBeforeTheLimit(t *testing.T) {
	const limit = accountMovementsDefaultLimit // what the probe handler's ParseLimit returns
	base := timescale.SEP41MovementsFloorLedger
	wm := base - 5_000 // the cap67 archive's watermark: the CH arm's ceiling
	when := time.Unix(1_700_000_000, 0).UTC()

	// The archive holds, newest first: 40 rows ABOVE the watermark (the
	// derive is mid-window — they are not servable yet) and 30 rows below
	// it (servable, and what the reader must page through).
	var archive []clickhouse.AccountMovementRow
	row := func(ledger uint32) clickhouse.AccountMovementRow {
		return clickhouse.AccountMovementRow{
			Address:         validTestAccount,
			Ledger:          ledger,
			LedgerCloseTime: when,
			TxHash:          validTestTxHash,
			OpIndex:         0,
			LegIndex:        0,
			Direction:       clickhouse.AccountMovementReceived,
			MovementKind:    "payment",
			Provenance:      "classic_derived",
			Asset:           "native",
			Amount:          big.NewInt(1000),
		}
	}
	for i := uint32(0); i < 40; i++ {
		archive = append(archive, row(wm+40-i)) // ledgers wm+40 … wm+1
	}
	for i := uint32(0); i < 30; i++ {
		archive = append(archive, row(wm-i)) // ledgers wm … wm-29
	}

	reader := &movementsArmReader{
		capReader: &capReader{probe: &deadlineProbe{}},
		wm:        wm,
		chRows:    archive,
	}

	view := callMovements(t, reader, &stubSEP41Tail{}, "")

	if len(view.Movements) != limit {
		t.Fatalf("served %d movements, want a FULL page of %d — the ceiling was applied to the page "+
			"AFTER the SQL LIMIT, so rows above the watermark ate every slot (F055)", len(view.Movements), limit)
	}
	for _, m := range view.Movements {
		if m.Ledger > wm {
			t.Fatalf("movement at ledger %d served above the cap67 watermark %d (F055)", m.Ledger, wm)
		}
	}
	if view.Movements[0].Ledger != wm {
		t.Fatalf("newest served movement is ledger %d, want %d (the highest servable row)", view.Movements[0].Ledger, wm)
	}
	if view.NextCursor == "" {
		t.Fatalf("next_cursor is empty on a full page — the account's pre-watermark history is unreachable (F055)")
	}
}

// TestAccountMovements_GenesisFloorFailsClosed pins the ceiling's
// set-signal: a deployment that installs the movements floor at genesis
// (testnet/futurenet, timescale.InstallMovementsFloor(1)) computes a
// ceiling of floor-1 == 0 whenever the cap67 watermark is absent or
// unreadable, and must then serve NOTHING from the ClickHouse arm — the
// Postgres tail covers the whole net. Carrying the clamp on a
// `MaxLedger > 0` sentinel instead of an explicit HasMaxLedger drops the
// predicate at exactly that ceiling and double-lists every transfer the
// tail already serves.
func TestAccountMovements_GenesisFloorFailsClosed(t *testing.T) {
	timescale.InstallMovementsFloor(1)
	t.Cleanup(func() { timescale.InstallMovementsFloor(0) })

	const ledger = 12_345 // a real testnet ledger: above the genesis floor, so the tail owns it
	when := time.Unix(1_700_000_000, 0).UTC()

	reader := &movementsArmReader{
		capReader: &capReader{probe: &deadlineProbe{}},
		wm:        0,
		wmErr:     errors.New("clickhouse: cap67 watermark: connection reset"),
		chRows: []clickhouse.AccountMovementRow{{
			Address:         validTestAccount,
			Ledger:          ledger,
			LedgerCloseTime: when,
			TxHash:          validTestTxHash,
			Direction:       clickhouse.AccountMovementReceived,
			MovementKind:    "transfer",
			Provenance:      "cap67_derived",
			Asset:           "USDC-" + validTestAccount,
			Amount:          big.NewInt(1000),
		}},
	}
	tail := &stubSEP41Tail{rows: []timescale.SEP41TransferRow{{
		ContractID: validTestContract,
		Ledger:     ledger, // the SAME transfer, served by the watched-token tail
		TxHash:     validTestTxHash,
		ObservedAt: when,
		ToAddr:     validTestAccount,
		Amount:     big.NewInt(1000),
	}}}

	view := callMovements(t, reader, tail, "")

	if !reader.gotFilter.HasMaxLedger || reader.gotFilter.MaxLedger != 0 {
		t.Fatalf("CH arm read with filter {MaxLedger:%d HasMaxLedger:%t}, want a SET ceiling of 0 — "+
			"at an installed genesis floor the ceiling is floor-1 == 0 and must still be sent",
			reader.gotFilter.MaxLedger, reader.gotFilter.HasMaxLedger)
	}
	if len(view.Movements) != 1 {
		t.Fatalf("served %d movements, want exactly 1 (the Postgres tail row) — a ceiling of 0 must serve "+
			"nothing from the ClickHouse arm at an installed genesis floor", len(view.Movements))
	}
	if got := view.Movements[0].Provenance; got != "cap67_event" {
		t.Fatalf("served provenance %q, want cap67_event (the Postgres tail row); the ClickHouse arm was "+
			"served unclamped at ceiling 0", got)
	}
}
