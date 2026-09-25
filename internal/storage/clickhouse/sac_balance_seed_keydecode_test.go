package clickhouse

import (
	"context"
	"encoding/base64"
	"math/big"
	"strings"
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

// An undecodable key that carries no watched contract id is skipped like any
// other non-watched row: the current-state scan reads every contract_data row
// network-wide, so one bad row elsewhere must not abort the seed.
func TestSACBalanceSeedFromRow_UndecodableUnwatchedKeySkipped(t *testing.T) {
	keys := map[string]string{
		"unwatched contract": corruptBalanceKeyXDR(t, otherSAC),
		"not base64":         "%%%not-base64%%%",
		"truncated prefix":   base64.StdEncoding.EncodeToString([]byte{0, 0, 0, 6, 0, 0, 0, 1, 0xAB}),
	}
	for name, keyXDR := range keys {
		for _, changeType := range []string{"created", "updated", "removed"} {
			_, matched, err := sacBalanceSeedFromRow(keyXDR, "", changeType, seedLedger, time.Now().UTC(), seedWatched())
			if err != nil {
				t.Errorf("%s/%s: err = %v, want a skip (nil error)", name, changeType, err)
			}
			if matched {
				t.Errorf("%s/%s: matched=true for an undecodable key", name, changeType)
			}
		}
	}
}

// A live row whose undecodable key carries a WATCHED contract id is lake
// corruption on the served-tier write path and still fails the seed, naming
// the contract. A removal is skipped, as the full-history reducer does.
func TestSACBalanceSeedFromRow_UndecodableWatchedKeyErrors(t *testing.T) {
	keyXDR := corruptBalanceKeyXDR(t, seedSAC)
	_, matched, err := sacBalanceSeedFromRow(keyXDR, "", "updated", seedLedger, time.Now().UTC(), seedWatched())
	if err == nil || !strings.Contains(err.Error(), seedSAC) {
		t.Errorf("err = %v, want a decode error naming watched contract %s", err, seedSAC)
	}
	if matched {
		t.Error("matched=true alongside a decode error")
	}
	if _, matched, err := sacBalanceSeedFromRow(keyXDR, "", "removed", seedLedger, time.Now().UTC(), seedWatched()); err != nil || matched {
		t.Errorf("removed: (matched=%v, err=%v), want a skip", matched, err)
	}
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
