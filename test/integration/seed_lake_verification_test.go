//go:build integration

package integration_test

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestSeedWalk_VerifyLakeRefusesAHole pins GH #713's coverage leg on both
// full-history seed readers: a walk asked to verify the lake must refuse,
// before emitting anything, a range whose stellar.ledgers rows are missing.
// The shared test lake carries entry changes at ledgers no fixture wrote a
// ledgers row for, so the hole here is real. Without the check both readers
// emit the holder / balance below as current state, which is what let a
// LiveSink drop elect a pre-hole value and stamp it full_history.
func TestSeedWalk_VerifyLakeRefusesAHole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		sac   = "CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY" // BLND SAC wrapper (real mainnet id)
		asset = "BLND:GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY"
		at    = uint32(31_000_000)
	)
	closeTime := time.Date(2021, 7, 1, 0, 0, 0, 0, time.UTC)
	sacContract := fhContractScAddr(t, sac)
	holder, holderAddr := fhSyntheticAccountAddr(t, 0xC7)
	holderKey := fhBalanceKey(t, holderAddr)
	keyXDR := fhKeyXDR(t, sacContract, holderKey)
	cbID := cbsID(t, 0x71)

	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: at, CloseTime: closeTime, OpIndex: -1, ChangeIndex: 1,
			ChangeType: "created", EntryType: "contract_data", KeyXDR: keyXDR,
			EntryXDR: fhEntryXDR(t, sacContract, holderKey, fhI128Val(big.NewInt(7_000_000)), at),
		},
		fhLiveTTLRow(keyXDR, at),
		{
			LedgerSeq: at, CloseTime: closeTime, TxHash: "slv01", IntraLedgerSeq: 1,
			ChangeType: "created", EntryType: "claimable_balance",
			KeyXDR:   cbsKeyXDR(t, cbID),
			EntryXDR: cbsEntryXDR(t, cbID, cbsAsset(t, "AQUA"), 5_000, at),
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	walk := chstore.SeedWalk{VerifyLake: true}

	var sacEmitted int
	ev, err := chstore.StreamSACBalanceSeedsFullHistory(ctx, addr, map[string]string{sac: asset}, walk, func(s chstore.SACBalanceSeed) error {
		if s.Holder == holder {
			sacEmitted++
		}
		return nil
	})
	if !errors.Is(err, chstore.ErrSeedLakeIncomplete) {
		t.Errorf("SAC full-history: err = %v, want ErrSeedLakeIncomplete", err)
	}
	if sacEmitted != 0 || ev.LakeVerifiedThrough != 0 {
		t.Errorf("SAC full-history: emitted %d seeds, LakeVerifiedThrough=%d over an unverified lake, want 0 and 0", sacEmitted, ev.LakeVerifiedThrough)
	}

	var cbEmitted int
	ev, err = chstore.StreamClaimableBalanceSeeds(ctx, addr, nil, nil, walk, func(s chstore.ClaimableBalanceSeed) error {
		if s.ClaimableID == cbsHex(cbID) {
			cbEmitted++
		}
		return nil
	})
	if !errors.Is(err, chstore.ErrSeedLakeIncomplete) {
		t.Errorf("claimable: err = %v, want ErrSeedLakeIncomplete", err)
	}
	if cbEmitted != 0 || ev.LakeVerifiedThrough != 0 {
		t.Errorf("claimable: emitted %d seeds, LakeVerifiedThrough=%d over an unverified lake, want 0 and 0", cbEmitted, ev.LakeVerifiedThrough)
	}
}
