package clickhouse

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// archivalConn answers the three queries archival resolution issues: the
// ttl_live_until existence probe, the live_until lookup (keyed by key_xdr, so
// fixtures need not hash), and the stellar.ledgers close-time lookup.
func archivalConn(t *testing.T, liveUntil map[string]uint32, closeTimes map[uint32]time.Time) *stubConn {
	t.Helper()
	ttlRows := make([][]any, 0, len(liveUntil))
	for k, lu := range liveUntil {
		h, err := TTLKeyHash(k)
		if err != nil {
			t.Fatalf("TTLKeyHash: %v", err)
		}
		ttlRows = append(ttlRows, []any{h, lu})
	}
	ledgerRows := make([][]any, 0, len(closeTimes))
	for seq, ct := range closeTimes {
		ledgerRows = append(ledgerRows, []any{seq, ct})
	}
	return &stubConn{respond: func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "EXISTS TABLE stellar.ttl_live_until"):
			return &stubRows{data: [][]any{{uint8(1)}}}, nil
		case strings.Contains(q, "FROM stellar.ttl_live_until"):
			return &stubRows{data: ttlRows}, nil
		case strings.Contains(q, "FROM stellar.ledgers"):
			return &stubRows{data: ledgerRows}, nil
		}
		t.Fatalf("unexpected query: %s", q)
		return nil, nil
	}}
}

func queriedLedgers(c *stubConn) bool {
	for _, q := range c.queries {
		if strings.Contains(q, "FROM stellar.ledgers") {
			return true
		}
	}
	return false
}

const (
	archLastWrite = uint32(62_400_123)
	archLiveUntil = uint32(62_500_000)
	archAsOf      = uint32(63_000_000)
)

var archCloseTime = time.Date(2025, 3, 1, 12, 0, 5, 0, time.UTC)

func liveSeed(t *testing.T, holder string, amount int64, ledger uint32) SACBalanceSeed {
	t.Helper()
	return SACBalanceSeed{
		ContractID: seedSAC,
		AssetKey:   seedAsset,
		Holder:     holder,
		Balance:    big.NewInt(amount),
		LedgerSeq:  ledger,
		CloseTime:  time.Date(2024, 11, 1, 0, 0, 0, 0, time.UTC),
		keyXDR:     mustKeyXDR(t, mustContractScAddr(t, seedSAC), seedBalanceKey(t, holder)),
	}
}

func collectLive(t *testing.T, conn driver.Conn, seeds []SACBalanceSeed) ([]SACBalanceSeed, error) {
	t.Helper()
	var got []SACBalanceSeed
	err := emitLiveSeeds(context.Background(), conn, seeds, archAsOf, func(s SACBalanceSeed) error {
		got = append(got, s)
		return nil
	})
	return got, err
}

// An archived current-state entry is retracted AT ITS ARCHIVAL LEDGER
// (live_until+1, with that ledger's close time), never at its last write: the
// seed's top-of-ledger intra seq would otherwise overwrite the genuine
// observation there and zero the holder across [last write, archival).
func TestEmitLiveSeeds_ArchivedTombstoneAtArchivalLedger(t *testing.T) {
	archived := liveSeed(t, seedHolder, 100_000_000, archLastWrite)
	live := liveSeed(t, seedSyntheticHolder(t, 0x42), 7, archLastWrite)
	conn := archivalConn(t,
		map[string]uint32{archived.keyXDR: archLiveUntil, live.keyXDR: archAsOf},
		map[uint32]time.Time{archLiveUntil + 1: archCloseTime})

	got, err := collectLive(t, conn, []SACBalanceSeed{archived, live})
	if err != nil {
		t.Fatalf("emitLiveSeeds: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("emitted %d seeds, want 2 (one tombstone, one live): %+v", len(got), got)
	}
	tomb, kept := got[0], got[1]
	if !tomb.IsRemoval || tomb.Balance.Sign() != 0 || tomb.Holder != seedHolder {
		t.Errorf("archived holder emitted as %+v, want an IsRemoval zero-balance tombstone for %s", tomb, seedHolder)
	}
	if tomb.LedgerSeq != archLiveUntil+1 {
		t.Errorf("tombstone LedgerSeq = %d, want archival ledger %d (live_until+1), not last write %d", tomb.LedgerSeq, archLiveUntil+1, archLastWrite)
	}
	if !tomb.CloseTime.Equal(archCloseTime) {
		t.Errorf("tombstone CloseTime = %v, want the archival ledger's close time %v", tomb.CloseTime, archCloseTime)
	}
	if kept.IsRemoval || kept.Balance.Cmp(big.NewInt(7)) != 0 || kept.LedgerSeq != archLastWrite {
		t.Errorf("live holder (live_until == asOf) emitted as %+v, want its unchanged balance", kept)
	}
}

// A live_until below the entry's own last write is impossible for real data
// (a write needs a live entry), so it cannot prove archival: fail open.
func TestEmitLiveSeeds_StaleTTLBelowLastWriteKeepsLive(t *testing.T) {
	s := liveSeed(t, seedHolder, 100, archLastWrite)
	conn := archivalConn(t, map[string]uint32{s.keyXDR: archLastWrite - 1}, nil)

	got, err := collectLive(t, conn, []SACBalanceSeed{s})
	if err != nil {
		t.Fatalf("emitLiveSeeds: %v", err)
	}
	if len(got) != 1 || got[0].IsRemoval || got[0].Balance.Cmp(big.NewInt(100)) != 0 || got[0].LedgerSeq != archLastWrite {
		t.Fatalf("got %+v, want the live seed unchanged", got)
	}
	if queriedLedgers(conn) {
		t.Error("looked up close times with nothing archived")
	}
}

// Without the archival ledger's close time there is no honest observed_at for
// the tombstone; the seed stops rather than invent one.
func TestEmitLiveSeeds_MissingArchivalLedgerErrors(t *testing.T) {
	s := liveSeed(t, seedHolder, 100, archLastWrite)
	conn := archivalConn(t, map[string]uint32{s.keyXDR: archLiveUntil}, nil)

	if _, err := collectLive(t, conn, []SACBalanceSeed{s}); err == nil || !strings.Contains(err.Error(), "62500001") {
		t.Fatalf("err = %v, want a missing-archival-ledger error naming ledger 62500001", err)
	}
}

// A removed current-state row is already a tombstone at its removal ledger;
// its TTL is not consulted.
func TestEmitLiveSeeds_RemovalPassesThroughWithoutTTL(t *testing.T) {
	s := liveSeed(t, seedHolder, 0, archLastWrite)
	s.IsRemoval = true
	conn := archivalConn(t, map[string]uint32{s.keyXDR: archLiveUntil}, map[uint32]time.Time{archLiveUntil + 1: archCloseTime})

	got, err := collectLive(t, conn, []SACBalanceSeed{s})
	if err != nil {
		t.Fatalf("emitLiveSeeds: %v", err)
	}
	if len(got) != 1 || !got[0].IsRemoval || got[0].LedgerSeq != archLastWrite {
		t.Fatalf("got %+v, want the removal tombstone at its removal ledger %d", got, archLastWrite)
	}
}

// The full-history reducer places an archived key's tombstone at the archival
// ledger too, replacing the stale live winner.
func TestSACSeedReducer_RetractArchivedAtArchivalLedger(t *testing.T) {
	keyXDR := seedKeyFor(t)
	r := newSACSeedReducer(seedWatched())
	ct := time.Date(2024, 11, 1, 0, 0, 0, 0, time.UTC)
	if err := r.offer(keyXDR, seedEntryFor(t, 100_000_000, archLastWrite), "updated", ct, seedOrd(archLastWrite, 3, "aa", 0, 1)); err != nil {
		t.Fatalf("offer: %v", err)
	}
	conn := archivalConn(t, map[string]uint32{keyXDR: archLiveUntil}, map[uint32]time.Time{archLiveUntil + 1: archCloseTime})

	n, err := r.retractArchived(context.Background(), conn, archAsOf)
	if err != nil {
		t.Fatalf("retractArchived: %v", err)
	}
	if n != 1 {
		t.Errorf("retracted %d keys, want 1", n)
	}
	got := collectSeeds(t, r)
	if len(got) != 1 {
		t.Fatalf("emitted %d seeds, want 1 tombstone: %+v", len(got), got)
	}
	if !got[0].IsRemoval || got[0].Balance.Sign() != 0 || got[0].Holder != seedHolder {
		t.Errorf("emitted %+v, want an IsRemoval zero-balance tombstone for %s", got[0], seedHolder)
	}
	if got[0].LedgerSeq != archLiveUntil+1 || !got[0].CloseTime.Equal(archCloseTime) {
		t.Errorf("tombstone at ledger %d / %v, want archival ledger %d / %v", got[0].LedgerSeq, got[0].CloseTime, archLiveUntil+1, archCloseTime)
	}
}
