//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sac_balances"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestSACEvictionFallsOutOfServedSupply is the served-money proof for Q119
// (audit-2026-09-02), end to end over real TimescaleDB:
//
//	LedgerCloseMeta with an evicted key
//	  → internal/dispatcher (eviction phase)
//	  → sac_balances observer
//	  → timescale.InsertSACBalanceObservation
//	  → SumSACBalancesAtOrBefore (the served SAC supply component)
//
// Soroban state archival is the one way a ledger entry leaves the live
// state without a transaction touching it, so nothing in transaction meta
// can report it. Before the eviction phase the archived balance stayed in
// the served supply FOREVER — the published supply drifted permanently
// above the truth and never self-corrected.
//
// The starting balance is written the way the ops SAC seed writes it, at
// timescale.SeedIntraLedgerSeq (MaxUint32, "authoritative reconstructed
// final state for its ledger"). That is the sentinel the read path's
// tie-break prefers, so it is the row an eviction has to beat: it beats it
// on LEDGER, which is what makes the fix hold against a re-seed of the
// dormant holder the seed exists to recover.
func TestSACEvictionFallsOutOfServedSupply(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		// Zero-body C-strkey / G-strkey fixtures: a SAC wrapper contract
		// and the holder whose balance ages out of the live state.
		sorobanContract = "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4"
		holderAccount   = "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF"
		assetKey        = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

		seedLedger    = uint32(56_400_000)
		evictLedger   = uint32(63_500_000)
		restoreLedger = uint32(63_600_000)
	)
	t0 := time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)

	observer, err := sac_balances.NewObserver(map[string]string{sorobanContract: assetKey})
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	disp := dispatcher.New()
	disp.AddEntryDecoder(observer)

	// ── The holder's last write, as the ops SAC seed lands it.
	if err := store.InsertSACBalanceObservation(ctx, timescale.SACBalanceObservation{
		ContractID:     sorobanContract,
		AssetKey:       assetKey,
		Holder:         holderAccount,
		Ledger:         seedLedger,
		ObservedAt:     t0,
		Balance:        big.NewInt(1_000_000),
		IntraLedgerSeq: timescale.SeedIntraLedgerSeq,
	}); err != nil {
		t.Fatalf("seed the dormant holder: %v", err)
	}
	if got := sumSAC(t, ctx, store, assetKey, restoreLedger); got.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Fatalf("served SAC component before eviction = %s, want 1000000", got)
	}

	// ── The ledger that archives it. No transactions: eviction is applied
	// at ledger close and belongs to no transaction.
	lcm := evictionOnlyLedger(evictLedger, []xdr.LedgerKey{
		evictedSACBalanceKey(t, sorobanContract, holderAccount),
	})
	evs, err := disp.ProcessLedger(lcm, testPassphrase)
	if err != nil {
		t.Fatalf("ProcessLedger(eviction): %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("eviction ledger produced %d observations, want 1", len(evs))
	}
	persistSAC(t, ctx, store, evs[0].(sac_balances.Observation))

	got := sumSAC(t, ctx, store, assetKey, restoreLedger)
	if got.Sign() != 0 {
		t.Fatalf("served SAC component after the eviction = %s, want 0 — the archived balance is still "+
			"published as live supply and nothing will ever take it back out", got)
	}

	// ── Archival is reversible: a restore must put the balance back, or the
	// fix has traded a permanent over-count for a permanent under-count.
	// Routed through the dispatcher's entry-change seam (the eviction path
	// above is what needed the LCM walk; a restore arrives as an ordinary
	// apply-phase change).
	restoreEvs, err := disp.RouteEntryChange(dispatcher.LedgerEntryChangeContext{
		Ledger:         restoreLedger,
		ClosedAt:       t0.Add(2 * time.Hour),
		OpIndex:        0,
		IntraLedgerSeq: 3,
		Change:         restoredSACBalanceChange(t, sorobanContract, holderAccount, 1_000_000),
	})
	if err != nil {
		t.Fatalf("RouteEntryChange(restore): %v", err)
	}
	if len(restoreEvs) != 1 {
		t.Fatalf("restore produced %d observations, want 1", len(restoreEvs))
	}
	persistSAC(t, ctx, store, restoreEvs[0].(sac_balances.Observation))

	if got := sumSAC(t, ctx, store, assetKey, restoreLedger); got.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Fatalf("served SAC component after the restore = %s, want 1000000 — an entry brought back out "+
			"of the archive is live state again", got)
	}
}

func sumSAC(t *testing.T, ctx context.Context, store *timescale.Store, assetKey string, asOf uint32) *big.Int {
	t.Helper()
	got, err := store.SumSACBalancesAtOrBefore(ctx, assetKey, asOf)
	if err != nil {
		t.Fatalf("SumSACBalancesAtOrBefore: %v", err)
	}
	return got
}

// persistSAC mirrors pipeline.persistSACBalanceObservation — the production
// sink for this observation type.
func persistSAC(t *testing.T, ctx context.Context, store *timescale.Store, o sac_balances.Observation) {
	t.Helper()
	if err := store.InsertSACBalanceObservation(ctx, timescale.SACBalanceObservation{
		ContractID:     o.ContractID,
		AssetKey:       o.AssetKey,
		Holder:         o.Holder,
		Ledger:         o.Ledger,
		ObservedAt:     o.ObservedAt,
		Balance:        o.Balance,
		IsRemoval:      o.IsRemoval,
		IntraLedgerSeq: o.IntraLedgerSeq,
	}); err != nil {
		t.Fatalf("InsertSACBalanceObservation %s/%s@%d: %v", o.ContractID, o.Holder, o.Ledger, err)
	}
}

// sacBalanceScKey builds the SEP-41 `Vec(Symbol("Balance"), Address)` key
// a SAC stores a holder's balance under.
func sacBalanceScKey(t *testing.T, holder string) xdr.ScVal {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteAccountID, holder)
	if err != nil {
		t.Fatalf("strkey.Decode(%q): %v", holder, err)
	}
	var pk [32]byte
	copy(pk[:], raw)
	accountID := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: (*xdr.Uint256)(&pk)}
	address := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &accountID}
	sym := xdr.ScSymbol("Balance")
	vec := xdr.ScVec{
		{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
		{Type: xdr.ScValTypeScvAddress, Address: &address},
	}
	vp := &vec
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vp}
}

func sacContractAddress(t *testing.T, contract string) xdr.ScAddress {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, contract)
	if err != nil {
		t.Fatalf("strkey.Decode(%q): %v", contract, err)
	}
	var id [32]byte
	copy(id[:], raw)
	contractID := xdr.ContractId(id)
	return xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &contractID}
}

// evictedSACBalanceKey is the ledger key core reports when a SAC balance
// entry's TTL lapses and it is archived out of the live state.
func evictedSACBalanceKey(t *testing.T, contract, holder string) xdr.LedgerKey {
	t.Helper()
	return xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   sacContractAddress(t, contract),
			Key:        sacBalanceScKey(t, holder),
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
}

func restoredSACBalanceChange(t *testing.T, contract, holder string, amount int64) xdr.LedgerEntryChange {
	t.Helper()
	return xdr.LedgerEntryChange{
		Type: xdr.LedgerEntryChangeTypeLedgerEntryRestored,
		Restored: &xdr.LedgerEntry{
			Data: xdr.LedgerEntryData{
				Type: xdr.LedgerEntryTypeContractData,
				ContractData: &xdr.ContractDataEntry{
					Contract:   sacContractAddress(t, contract),
					Key:        sacBalanceScKey(t, holder),
					Durability: xdr.ContractDataDurabilityPersistent,
					Val: xdr.ScVal{
						Type: xdr.ScValTypeScvI128,
						I128: &xdr.Int128Parts{Hi: 0, Lo: xdr.Uint64(amount)},
					},
				},
			},
		},
	}
}

// evictionOnlyLedger is a transaction-free ledger that archives the given
// keys at close — the shape of a ledger in which a dormant balance ages out.
func evictionOnlyLedger(seq uint32, keys []xdr.LedgerKey) xdr.LedgerCloseMeta {
	return xdr.LedgerCloseMeta{
		V: 1,
		V1: &xdr.LedgerCloseMetaV1{
			LedgerHeader: xdr.LedgerHeaderHistoryEntry{
				Header: xdr.LedgerHeader{
					LedgerSeq: xdr.Uint32(seq),
					ScpValue:  xdr.StellarValue{CloseTime: xdr.TimePoint(1_758_270_000)},
				},
			},
			TxSet: xdr.GeneralizedTransactionSet{
				V:       1,
				V1TxSet: &xdr.TransactionSetV1{},
			},
			EvictedKeys: keys,
		},
	}
}
