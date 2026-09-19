package sac_balances

import (
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

const evictionPassphrase = "Test SDF Network ; September 2015"

// evictedBalanceKey is the ledger key core reports when a SAC balance
// entry's TTL lapses and it is archived out of the live state.
func evictedBalanceKey(t *testing.T, contract string, holder string) xdr.LedgerKey {
	t.Helper()
	cid := mustContractID(t, contract)
	return xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract: xdr.ScAddress{
				Type:       xdr.ScAddressTypeScAddressTypeContract,
				ContractId: (*xdr.ContractId)(&cid),
			},
			Key:        makeBalanceKey(t, holder),
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
}

// evictionLedger is a transaction-free ledger that archives the given keys
// at close — the shape of a ledger in which a dormant balance ages out.
func evictionLedger(seq uint32, keys []xdr.LedgerKey) xdr.LedgerCloseMeta {
	return xdr.LedgerCloseMeta{
		V: 1,
		V1: &xdr.LedgerCloseMetaV1{
			LedgerHeader: xdr.LedgerHeaderHistoryEntry{
				Header: xdr.LedgerHeader{
					LedgerSeq: xdr.Uint32(seq),
					ScpValue:  xdr.StellarValue{CloseTime: xdr.TimePoint(1_700_000_000)},
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

// TestObserver_EvictedBalanceIsObservedAsRemoval pins the eviction half of
// the Soroban state-archival lifecycle end to end through the real
// dispatcher (Q119, audit-2026-09-02).
//
// A SAC balance whose TTL lapses is archived at ledger close. No
// transaction touches it, so it appears in no transaction meta and the
// observer used to never hear about it: the holder's last write stood as
// their current balance forever and the served SAC supply component stayed
// permanently above the truth. The observer must see it as a removal — the
// same absorbing "no longer live state" a deleted entry produces.
func TestObserver_EvictedBalanceIsObservedAsRemoval(t *testing.T) {
	const assetKey = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	o, err := NewObserver(map[string]string{cSAC: assetKey})
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	d := dispatcher.New()
	d.AddEntryDecoder(o)

	lcm := evictionLedger(63_500_000, []xdr.LedgerKey{evictedBalanceKey(t, cSAC, gHolder)})
	evs, err := d.ProcessLedger(lcm, evictionPassphrase)
	if err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("the evicted SAC balance produced %d observations, want 1 — "+
			"an archived entry that is never observed stays in the served supply forever", len(evs))
	}
	obs, ok := evs[0].(Observation)
	if !ok {
		t.Fatalf("observation is %T, want sac_balances.Observation", evs[0])
	}
	if !obs.IsRemoval {
		t.Error("eviction observed with IsRemoval=false — the read path only excludes removals, " +
			"so the archived balance would keep counting toward supply")
	}
	if obs.Balance == nil || obs.Balance.Sign() != 0 {
		t.Errorf("eviction observed with balance %v, want 0", obs.Balance)
	}
	if obs.AssetKey != assetKey || obs.ContractID != cSAC || obs.Holder != gHolder {
		t.Errorf("eviction observed for %s/%s/%s, want %s/%s/%s",
			obs.AssetKey, obs.ContractID, obs.Holder, assetKey, cSAC, gHolder)
	}
	if obs.Ledger != 63_500_000 {
		t.Errorf("eviction observed at ledger %d, want 63500000 — the eviction ledger is what ranks it "+
			"above the holder's last write", obs.Ledger)
	}
}

// TestObserver_UnwatchedEvictionIsIgnored keeps the eviction phase from
// widening the observer's surface: the dispatcher hands it every key the
// ledger archived, and a contract outside the operator's SAC wrapper map
// must still produce nothing.
func TestObserver_UnwatchedEvictionIsIgnored(t *testing.T) {
	o, err := NewObserver(map[string]string{cSAC: "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"})
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	d := dispatcher.New()
	d.AddEntryDecoder(o)

	const unwatched = "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"
	lcm := evictionLedger(63_500_001, []xdr.LedgerKey{evictedBalanceKey(t, unwatched, gHolder)})
	evs, err := d.ProcessLedger(lcm, evictionPassphrase)
	if err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("an unwatched contract's eviction produced %d observations, want 0", len(evs))
	}
}

// TestObserver_RestoreAfterEvictionReturnsTheBalance pins the round trip:
// archival is reversible, so the Restored change must put the balance back
// rather than leaving the holder zeroed. Without this the eviction fix
// would trade a permanent over-count for a permanent under-count.
func TestObserver_RestoreAfterEvictionReturnsTheBalance(t *testing.T) {
	const assetKey = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	o, err := NewObserver(map[string]string{cSAC: assetKey})
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	restored := makeContractDataChange(t, cSAC, makeBalanceKey(t, gHolder), makeI128Val(1_000_000))
	restored.Type = xdr.LedgerEntryChangeTypeLedgerEntryRestored
	restored.Restored, restored.Updated = restored.Updated, nil

	evs, err := o.Decode(dispatcher.LedgerEntryChangeContext{
		Ledger:  63_500_002,
		OpIndex: 0,
		Change:  restored,
	})
	if err != nil {
		t.Fatalf("Decode(Restored): %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("restore produced %d observations, want 1", len(evs))
	}
	obs, ok := evs[0].(Observation)
	if !ok {
		t.Fatalf("observation is %T, want sac_balances.Observation", evs[0])
	}
	if obs.IsRemoval {
		t.Error("restore observed as a removal — an entry brought back out of the archive is live state again")
	}
	if obs.Balance.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Errorf("restore observed balance %s, want 1000000", obs.Balance)
	}
}
