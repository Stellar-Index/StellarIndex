package clickhouse

import (
	"context"
	"encoding/base64"
	"math/big"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// corruptBalanceKeyXDR is a contract_data LedgerKey for contractStrkey whose
// XDR is truncated just past the contract hash: the contract is still
// readable from the raw prefix, but the key no longer decodes.
func corruptBalanceKeyXDR(t *testing.T, contractStrkey string) string {
	t.Helper()
	valid := mustKeyXDR(t, mustContractScAddr(t, contractStrkey), seedBalanceKey(t, seedHolder))
	raw, err := base64.StdEncoding.DecodeString(valid)
	if err != nil {
		t.Fatalf("decode fixture key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw[:44])
}

// The current-state stream keeps going past a corrupt unwatched row and still
// emits the watched holder behind it.
func TestStreamCurrentStateSeeds_UnwatchedCorruptKeyDoesNotAbort(t *testing.T) {
	ct := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	rows := &stubRows{data: [][]any{
		{corruptBalanceKeyXDR(t, otherSAC), "", "updated", seedLedger, ct},
		{seedKeyFor(t), seedEntryFor(t, 100_000_000, seedLedger), "updated", seedLedger, ct},
	}}
	conn := archivalConn(t, map[string]uint32{seedKeyFor(t): seedLedger + 1_000}, nil)

	var got []SACBalanceSeed
	err := streamCurrentStateSeeds(context.Background(), conn, driver.Rows(rows), seedWatched(), seedLedger+1, func(s SACBalanceSeed) error {
		got = append(got, s)
		return nil
	})
	if err != nil {
		t.Fatalf("streamCurrentStateSeeds aborted on an unwatched corrupt key: %v", err)
	}
	if len(got) != 1 || got[0].Holder != seedHolder || got[0].Balance.Cmp(big.NewInt(100_000_000)) != 0 {
		t.Fatalf("got %+v, want the watched holder's 100000000 balance", got)
	}
}
