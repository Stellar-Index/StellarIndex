//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestSACSeed_ArchivedHolderRetractedAtArchivalLedger drives both SAC seed
// readers against a real ClickHouse: a watched Balance entry written at
// lastWrite whose TTL lapsed at liveUntil must come back as a tombstone at
// liveUntil+1 carrying that ledger's stellar.ledgers close time — not at
// lastWrite, where the seed's top-of-ledger upsert would overwrite the genuine
// observation and zero the holder across [lastWrite, archival).
func TestSACSeed_ArchivedHolderRetractedAtArchivalLedger(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		lastWrite = uint32(31_100_000)
		liveUntil = uint32(31_200_000)
		tipLedger = uint32(31_300_000)
		asset     = "ARCH:GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"
	)
	archivalClose := time.Date(2025, 3, 1, 12, 0, 5, 0, time.UTC)

	sac, sacContract := fhSyntheticContractAddr(t, 0xE5)
	holder, holderAddr := fhSyntheticAccountAddr(t, 0xE6)
	balanceKey := fhBalanceKey(t, holderAddr)
	keyXDR := fhKeyXDR(t, sacContract, balanceKey)

	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: lastWrite, CloseTime: time.Date(2024, 11, 1, 0, 0, 0, 0, time.UTC),
			TxHash: "arch-seed-it", OpIndex: 0, ChangeIndex: 0,
			ChangeType: "created", EntryType: "contract_data", KeyXDR: keyXDR,
			EntryXDR: fhEntryXDR(t, sacContract, balanceKey, fhI128Val(big.NewInt(5_000_000)), lastWrite),
		},
		ttlChangeRow(keyXDR, lastWrite, 1, liveUntil, 48),
		// Lifts the lake tip (the seed's as-of ledger) past liveUntil.
		ttlChangeRow(ttlGovernedKeyXDR("arch-seed-tip"), tipLedger, 1, tipLedger+1_000_000, 48),
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	lb, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.ledgers (ledger_seq, close_time, ledger_hash, prev_hash, protocol_version)`)
	if err != nil {
		t.Fatalf("prepare ledgers: %v", err)
	}
	if err := lb.Append(liveUntil+1, archivalClose, fmt.Sprintf("%064d", liveUntil+1), "00", uint32(22)); err != nil {
		t.Fatalf("append ledger: %v", err)
	}
	if err := lb.Send(); err != nil {
		t.Fatalf("send ledgers: %v", err)
	}

	watched := map[string]string{sac: asset}
	readers := map[string]func(context.Context, string, map[string]string, func(chstore.SACBalanceSeed) error) error{
		"current-state": chstore.StreamSACBalanceSeeds,
		"full-history":  chstore.StreamSACBalanceSeedsFullHistory,
	}
	for name, stream := range readers {
		var got []chstore.SACBalanceSeed
		if err := stream(ctx, addr, watched, func(s chstore.SACBalanceSeed) error {
			if s.Holder == holder {
				got = append(got, s)
			}
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != 1 {
			t.Fatalf("%s: emitted %d seeds for the archived holder, want 1 tombstone: %+v", name, len(got), got)
		}
		s := got[0]
		if !s.IsRemoval || s.Balance.Sign() != 0 {
			t.Errorf("%s: emitted %+v, want an IsRemoval zero-balance tombstone", name, s)
		}
		if s.LedgerSeq != liveUntil+1 {
			t.Errorf("%s: tombstone LedgerSeq = %d, want archival ledger %d (not last write %d)", name, s.LedgerSeq, liveUntil+1, lastWrite)
		}
		if !s.CloseTime.Equal(archivalClose) {
			t.Errorf("%s: tombstone CloseTime = %v, want %v from stellar.ledgers", name, s.CloseTime, archivalClose)
		}
	}
}

// TestSACSeed_UncoveredTTLRefuses: a watched Balance entry with no
// stellar.ttl_live_until row (an unbackfilled projection) must fail both
// readers — never be emitted as a live holder under a clean-looking pass.
func TestSACSeed_UncoveredTTLRefuses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		written = uint32(31_400_000)
		asset   = "NOTTL:GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"
	)
	sac, sacContract := fhSyntheticContractAddr(t, 0xF1)
	holder, holderAddr := fhSyntheticAccountAddr(t, 0xF2)
	balanceKey := fhBalanceKey(t, holderAddr)
	keyXDR := fhKeyXDR(t, sacContract, balanceKey)

	rows := []chstore.LedgerEntryChangeRow{{
		LedgerSeq: written, CloseTime: time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC),
		TxHash: "nottl-seed-it", OpIndex: 0, ChangeIndex: 0,
		ChangeType: "created", EntryType: "contract_data", KeyXDR: keyXDR,
		EntryXDR: fhEntryXDR(t, sacContract, balanceKey, fhI128Val(big.NewInt(9_000_000)), written),
	}}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	watched := map[string]string{sac: asset}
	readers := map[string]func(context.Context, string, map[string]string, func(chstore.SACBalanceSeed) error) error{
		"current-state": chstore.StreamSACBalanceSeeds,
		"full-history":  chstore.StreamSACBalanceSeedsFullHistory,
	}
	for name, stream := range readers {
		var emitted int
		err := stream(ctx, addr, watched, func(s chstore.SACBalanceSeed) error {
			if s.Holder == holder {
				emitted++
			}
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "no stellar.ttl_live_until row") {
			t.Errorf("%s: err = %v, want the TTL-coverage refusal", name, err)
		}
		if emitted != 0 {
			t.Errorf("%s: emitted %d seeds for the uncovered holder, want 0", name, emitted)
		}
	}
}
