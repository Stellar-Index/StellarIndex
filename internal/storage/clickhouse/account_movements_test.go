package clickhouse

import (
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestFanOutAccountMovement_TwoParticipants(t *testing.T) {
	closeTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	m := AccountMovement{
		MovementKind:    "payment",
		Provenance:      "classic_derived",
		Ledger:          100,
		LedgerCloseTime: closeTime,
		TxHash:          "txhash",
		OpIndex:         1,
		LegIndex:        0,
		Asset:           "native",
		Amount:          big.NewInt(500),
		FromAddress:     "GALICE",
		ToAddress:       "GBOB",
	}
	rows := FanOutAccountMovement(m)
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}

	var sent, received *AccountMovementRow
	for i := range rows {
		switch rows[i].Direction {
		case AccountMovementSent:
			sent = &rows[i]
		case AccountMovementReceived:
			received = &rows[i]
		case AccountMovementSelf:
			t.Fatalf("unexpected 'self' direction for a two-participant movement: %+v", rows[i])
		}
	}
	if sent == nil || received == nil {
		t.Fatalf("expected one sent + one received row, got %+v", rows)
	}
	if sent.Address != "GALICE" || sent.Counterparty != "GBOB" {
		t.Errorf("sent row = %+v, want address=GALICE counterparty=GBOB", sent)
	}
	if received.Address != "GBOB" || received.Counterparty != "GALICE" {
		t.Errorf("received row = %+v, want address=GBOB counterparty=GALICE", received)
	}
	// Shared movement identity fields must be preserved on both fan-out rows.
	for _, r := range rows {
		if r.Ledger != 100 || r.TxHash != "txhash" || r.OpIndex != 1 || r.LegIndex != 0 {
			t.Errorf("row %+v lost shared movement identity", r)
		}
		if r.MovementKind != "payment" || r.Asset != "native" || r.Amount.Cmp(big.NewInt(500)) != 0 {
			t.Errorf("row %+v lost shared movement data", r)
		}
	}
}

func TestFanOutAccountMovement_SelfPayment(t *testing.T) {
	m := AccountMovement{
		MovementKind: "payment",
		Ledger:       100,
		TxHash:       "txhash",
		Asset:        "native",
		Amount:       big.NewInt(1),
		FromAddress:  "GSELF",
		ToAddress:    "GSELF",
	}
	rows := FanOutAccountMovement(m)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1 (self-payment must not double-count)", len(rows))
	}
	if rows[0].Direction != AccountMovementSelf {
		t.Errorf("direction = %q, want %q", rows[0].Direction, AccountMovementSelf)
	}
	if rows[0].Address != "GSELF" {
		t.Errorf("address = %q, want GSELF", rows[0].Address)
	}
	if rows[0].Counterparty != "" {
		t.Errorf("counterparty = %q, want empty for a self row", rows[0].Counterparty)
	}
}

func TestFanOutAccountMovement_SingleParticipant_ClaimableBalanceCreate(t *testing.T) {
	// claimable_balance_create: creator known, no claimant resolved yet.
	m := AccountMovement{
		MovementKind: "claimable_balance_create",
		Ledger:       100,
		TxHash:       "txhash",
		Asset:        "native",
		Amount:       big.NewInt(10),
		FromAddress:  "GCREATOR",
		ToAddress:    "",
	}
	rows := FanOutAccountMovement(m)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].Address != "GCREATOR" || rows[0].Direction != AccountMovementSent || rows[0].Counterparty != "" {
		t.Errorf("row = %+v, want address=GCREATOR direction=sent counterparty=''", rows[0])
	}
}

func TestFanOutAccountMovement_SingleParticipant_ClaimableBalanceClaim(t *testing.T) {
	// claimable_balance_claim: escrow side unknown (not a G-account), claimant known.
	m := AccountMovement{
		MovementKind: "claimable_balance_claim",
		Ledger:       100,
		TxHash:       "txhash",
		Asset:        "native",
		Amount:       big.NewInt(10),
		FromAddress:  "",
		ToAddress:    "GCLAIMANT",
	}
	rows := FanOutAccountMovement(m)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].Address != "GCLAIMANT" || rows[0].Direction != AccountMovementReceived || rows[0].Counterparty != "" {
		t.Errorf("row = %+v, want address=GCLAIMANT direction=received counterparty=''", rows[0])
	}
}

func TestFanOutAccountMovement_LiquidityPoolLegs(t *testing.T) {
	// Deposit leg: depositor known ("sent" into the pool), pool side unknown.
	deposit := AccountMovement{
		MovementKind: "liquidity_pool_deposit",
		Ledger:       100,
		TxHash:       "txhash",
		LegIndex:     0,
		Asset:        "native",
		Amount:       big.NewInt(10),
		FromAddress:  "GDEPOSITOR",
		ToAddress:    "",
	}
	depositRows := FanOutAccountMovement(deposit)
	if len(depositRows) != 1 || depositRows[0].Direction != AccountMovementSent {
		t.Fatalf("deposit leg rows = %+v, want 1 sent row", depositRows)
	}

	// Withdraw leg: withdrawer known ("received" from the pool), pool side unknown.
	withdraw := AccountMovement{
		MovementKind: "liquidity_pool_withdraw",
		Ledger:       100,
		TxHash:       "txhash",
		LegIndex:     1,
		Asset:        "native",
		Amount:       big.NewInt(10),
		FromAddress:  "",
		ToAddress:    "GWITHDRAWER",
	}
	withdrawRows := FanOutAccountMovement(withdraw)
	if len(withdrawRows) != 1 || withdrawRows[0].Direction != AccountMovementReceived {
		t.Fatalf("withdraw leg rows = %+v, want 1 received row", withdrawRows)
	}
}

func TestFanOutAccountMovement_NoParticipants(t *testing.T) {
	m := AccountMovement{MovementKind: "payment", Ledger: 1, TxHash: "tx", Asset: "native", Amount: big.NewInt(1)}
	rows := FanOutAccountMovement(m)
	if rows != nil {
		t.Errorf("rows = %+v, want nil for a movement with no known participant", rows)
	}
}

func TestFanOutAccountMovement_NilAmountPreserved(t *testing.T) {
	// Amount is passed through as-is by the fan-out (nil-guarding happens at
	// insert time, not fan-out time) — assert the fan-out doesn't panic or
	// silently substitute a value.
	m := AccountMovement{
		MovementKind: "payment",
		Ledger:       1,
		TxHash:       "tx",
		Asset:        "native",
		FromAddress:  "GALICE",
		ToAddress:    "GBOB",
	}
	rows := FanOutAccountMovement(m)
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.Amount != nil {
			t.Errorf("row.Amount = %v, want nil (unset) preserved from input", r.Amount)
		}
	}
}

func TestMarshalAccountMovementAttributes(t *testing.T) {
	got, err := marshalAccountMovementAttributes(nil)
	if err != nil || got != "{}" {
		t.Errorf("marshalAccountMovementAttributes(nil) = %q, %v; want '{}', nil", got, err)
	}
	got, err = marshalAccountMovementAttributes(map[string]any{"balance_id": "abc"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got != `{"balance_id":"abc"}` {
		t.Errorf("marshal = %q, want {\"balance_id\":\"abc\"}", got)
	}
}

func TestChunkStrings(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		got := chunkStrings(nil, 2000)
		if got != nil {
			t.Errorf("chunkStrings(nil, 2000) = %+v, want nil", got)
		}
		got = chunkStrings([]string{}, 2000)
		if got != nil {
			t.Errorf("chunkStrings([], 2000) = %+v, want nil", got)
		}
	})

	t.Run("exact multiple", func(t *testing.T) {
		ids := make([]string, 6)
		for i := range ids {
			ids[i] = string(rune('a' + i))
		}
		got := chunkStrings(ids, 2)
		if len(got) != 3 {
			t.Fatalf("len(chunks) = %d, want 3", len(got))
		}
		for i, c := range got {
			if len(c) != 2 {
				t.Errorf("chunk %d len = %d, want 2", i, len(c))
			}
		}
		if got[0][0] != "a" || got[0][1] != "b" || got[2][0] != "e" || got[2][1] != "f" {
			t.Errorf("chunks = %+v, want contiguous split preserving order", got)
		}
	})

	t.Run("remainder", func(t *testing.T) {
		ids := []string{"a", "b", "c", "d", "e"}
		got := chunkStrings(ids, 2)
		if len(got) != 3 {
			t.Fatalf("len(chunks) = %d, want 3", len(got))
		}
		if len(got[0]) != 2 || len(got[1]) != 2 || len(got[2]) != 1 {
			t.Errorf("chunk lens = [%d %d %d], want [2 2 1]", len(got[0]), len(got[1]), len(got[2]))
		}
		if got[2][0] != "e" {
			t.Errorf("last chunk = %+v, want [e]", got[2])
		}
	})

	t.Run("n bigger than input", func(t *testing.T) {
		ids := []string{"a", "b", "c"}
		got := chunkStrings(ids, 2000)
		if len(got) != 1 || len(got[0]) != 3 {
			t.Fatalf("chunkStrings(3 ids, 2000) = %+v, want one chunk of 3", got)
		}
	})

	t.Run("n<=0 means no chunking", func(t *testing.T) {
		ids := []string{"a", "b", "c"}
		got := chunkStrings(ids, 0)
		if len(got) != 1 || len(got[0]) != 3 {
			t.Fatalf("chunkStrings(3 ids, 0) = %+v, want one chunk of 3", got)
		}
	})
}

// TestSortAccountMovementRowsForInsert pins ledger-first batch order (then the table's ORDER BY
// columns): address-first made a partially sent batch span the window, which max(ledger) resume skipped.
func TestSortAccountMovementRowsForInsert(t *testing.T) {
	rows := []AccountMovementRow{
		{Address: "GB", Ledger: 2, TxHash: "z"},
		{Address: "GA", Ledger: 5, TxHash: "a"},
		{Address: "GA", Ledger: 1, TxHash: "b"},
		{Address: "GA", Ledger: 2, TxHash: "b", OpIndex: 1},
		{Address: "GA", Ledger: 2, TxHash: "b", OpIndex: 0},
	}
	sortAccountMovementRowsForInsert(rows)

	type key struct {
		addr    string
		ledger  uint32
		tx      string
		opIndex uint32
	}
	want := []key{
		{"GA", 1, "b", 0},
		{"GA", 2, "b", 0},
		{"GA", 2, "b", 1},
		{"GB", 2, "z", 0},
		{"GA", 5, "a", 0},
	}
	for i, w := range want {
		got := key{rows[i].Address, rows[i].Ledger, rows[i].TxHash, rows[i].OpIndex}
		if got != w {
			t.Errorf("rows[%d] = %+v, want %+v (ledger, then address, tx_hash, op_index)", i, got, w)
		}
	}
}

// TestAccountMovementsQueryDedupKeepsNewestVersion pins that LIMIT 1 BY keeps the newest ingested_at per key
// in every query shape; without the version in ORDER BY the surviving duplicate is unspecified.
func TestAccountMovementsQueryDedupKeepsNewestVersion(t *testing.T) {
	const want = "leg_index DESC, ingested_at DESC LIMIT 1 BY ledger, tx_hash, op_index, leg_index"
	for _, hasCursor := range []bool{false, true} {
		for _, f := range []AccountMovementFilter{
			{},
			{Kind: "payment", Direction: AccountMovementSent, Asset: "native", HasMaxLedger: true, MaxLedger: 1},
		} {
			if q := accountMovementsQuery(f, hasCursor); !strings.Contains(q, want) {
				t.Errorf("accountMovementsQuery(%+v, cursor=%v) does not order the dedup by version (want %q):\n%s", f, hasCursor, want, q)
			}
		}
	}
}

// TestAccountMovementsQuery_LedgerCeiling pins that the /movements merge boundary is a SQL predicate keyed
// on HasMaxLedger, never MaxLedger != 0: ceiling 0 is reachable (genesis floor, absent watermark) and must
// serve nothing.
func TestAccountMovementsQuery_LedgerCeiling(t *testing.T) {
	const clause = " AND ledger <= ?"

	t.Run("ceiling zero still emits the clause", func(t *testing.T) {
		q := accountMovementsQuery(AccountMovementFilter{HasMaxLedger: true, MaxLedger: 0}, false)
		if !strings.Contains(q, clause) {
			t.Errorf("accountMovementsQuery with a SET ceiling of 0 is missing %q — the CH arm would serve "+
				"its whole archive at an installed genesis floor (F055):\n%s", clause, q)
		}
	})

	t.Run("set ceiling emits the clause", func(t *testing.T) {
		q := accountMovementsQuery(AccountMovementFilter{HasMaxLedger: true, MaxLedger: 58_000_000}, true)
		if !strings.Contains(q, clause) {
			t.Errorf("accountMovementsQuery with a set ceiling is missing %q:\n%s", clause, q)
		}
	})

	t.Run("unset ceiling omits the clause", func(t *testing.T) {
		q := accountMovementsQuery(AccountMovementFilter{}, false)
		if strings.Contains(q, clause) {
			t.Errorf("accountMovementsQuery with no ceiling emitted %q — an unbounded read must stay "+
				"unbounded:\n%s", clause, q)
		}
	})

	t.Run("clause order mirrors arg order", func(t *testing.T) {
		q := accountMovementsQuery(AccountMovementFilter{
			Kind: "payment", Direction: AccountMovementSent, Asset: "native",
			HasMaxLedger: true, MaxLedger: 58_000_000,
		}, true)
		assetIdx := strings.Index(q, "asset = ?")
		ledgerIdx := strings.Index(q, clause)
		curIdx := strings.Index(q, "(ledger, tx_hash, op_index, leg_index) < (?, ?, ?, ?)")
		if !(assetIdx >= 0 && ledgerIdx > assetIdx && curIdx > ledgerIdx) {
			t.Errorf("the ledger ceiling must bind between the asset filter and the cursor keyset "+
				"(ExplorerReader.AccountMovements appends its args in that order):\n%s", q)
		}
	})

	t.Run("the ceiling precedes the LIMIT", func(t *testing.T) {
		q := accountMovementsQuery(AccountMovementFilter{HasMaxLedger: true, MaxLedger: 58_000_000}, false)
		if strings.Index(q, clause) > strings.Index(q, "LIMIT") {
			t.Errorf("the ledger ceiling must be a WHERE predicate, applied before the LIMIT (F055):\n%s", q)
		}
	})
}

// TestInsertAccountMovementsOrder_PartialSendLeavesNoGap pins that a partly sent batch is safe for
// max(ledger) resume: at every cut point each ledger below the highest written is complete. An address-first
// order fails.
func TestInsertAccountMovementsOrder_PartialSendLeavesNoGap(t *testing.T) {
	// A window shaped like a real one: many addresses, each active
	// across the whole ledger range.
	const (
		addresses = 12
		ledgerLo  = 1000
		ledgerHi  = 1040
	)
	var rows []AccountMovementRow
	for a := 0; a < addresses; a++ {
		for l := uint32(ledgerLo); l <= ledgerHi; l++ {
			rows = append(rows, AccountMovementRow{
				Address:   fmt.Sprintf("G%02d", a),
				Ledger:    l,
				TxHash:    fmt.Sprintf("%02d-%d", a, l),
				Direction: AccountMovementSent,
			})
		}
	}

	sortAccountMovementRowsForInsert(rows)

	// total[l] is how many rows the window holds for ledger l.
	total := map[uint32]int{}
	for _, r := range rows {
		total[r.Ledger]++
	}

	// Walk every cut point a failed chunk send could leave behind.
	written := map[uint32]int{}
	for cut, r := range rows {
		written[r.Ledger]++
		highest := uint32(0)
		for l := range written {
			if l > highest {
				highest = l
			}
		}
		for l := uint32(ledgerLo); l < highest; l++ {
			if written[l] != total[l] {
				t.Fatalf("a send that stopped after row %d left ledger %d with %d of %d rows while ledger %d "+
					"was already written: -resume restarts at %d and never revisits %d — a silent gap (RLT-296)",
					cut+1, l, written[l], total[l], highest, highest, l)
			}
		}
	}
}
