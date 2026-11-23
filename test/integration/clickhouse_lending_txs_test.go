//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestLendingReservesAtTxs_EachTxOwnLastImage: two transactions of one ledger write one reserve at different rates; the read
// returns each transaction's own last image (its highest (op_index, change_index)), unmoved by a duplicate lake row, keyed by
// the lowercase tx hash, and nothing for a transaction that was not asked for.
func TestLendingReservesAtTxs_EachTxOwnLastImage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")
	reader, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	const ledger = 88_200_000
	const base = 1_000_000_000_000
	var poolID, assetID xdr.ContractId
	poolID[0], assetID[0] = 0x81, 0x82
	pool := strkey.MustEncode(strkey.VersionByteContract, poolID[:])
	asset := strkey.MustEncode(strkey.VersionByteContract, assetID[:])
	key := resDataKeyB64(t, poolID, assetID)
	txA, txB, txC := strings.Repeat("a1", 32), strings.Repeat("b2", 32), strings.Repeat("c3", 32)
	closeAt := time.Date(2027, 4, 1, 12, 0, 0, 0, time.UTC)
	insert := func(tx string, op, change uint32, changeType string, bRate uint64) {
		t.Helper()
		if err := conn.Exec(ctx, `INSERT INTO stellar.ledger_entry_changes
			(ledger_seq, close_time, tx_hash, op_index, change_index, change_type, entry_type, key_xdr, entry_xdr, intra_ledger_seq)
			VALUES (?, ?, ?, ?, ?, ?, 'contract_data', ?, ?, 1)`,
			ledger, closeAt, tx, op, change, changeType, key, resDataEntryB64(t, poolID, assetID, ledger, bRate)); err != nil {
			t.Fatalf("insert %s op %d change %d: %v", tx[:2], op, change, err)
		}
	}
	insert(txA, 0, 0, "updated", base+1)
	insert(txA, 1, 0, "state", base+90)  // a pre-image, never the stored state
	insert(txA, 1, 1, "updated", base+2) // txA's last image
	insert(txA, 1, 1, "updated", base+2) // the lake's unmerged duplicate of it
	insert(txB, 0, 0, "updated", base+3)
	insert(txC, 0, 0, "updated", base+4)

	got, err := reader.LendingReservesAtTxs(ctx, pool, chstore.LendingBlendV2, []string{asset},
		[]chstore.LendingTxRef{{Ledger: ledger, TxHash: txA}, {Ledger: ledger, TxHash: txB}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]uint64{txA: base + 2, txB: base + 3}
	if len(got) != len(want) {
		t.Fatalf("got %d reserve states %v, want %d (txA, txB)", len(got), got, len(want))
	}
	for tx, bRate := range want {
		rd, ok := got[chstore.LendingTxReserve{LendingTxRef: chstore.LendingTxRef{Ledger: ledger, TxHash: tx}, Asset: asset}]
		if !ok || rd.BRate == nil || !rd.BRate.IsUint64() || rd.BRate.Uint64() != bRate {
			t.Errorf("tx %s…: state %+v (found %v), want b_rate %d", tx[:4], rd, ok, bRate)
		}
	}
}
