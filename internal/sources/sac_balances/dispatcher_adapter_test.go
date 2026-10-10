package sac_balances

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

const (
	// Valid C-strkey (zero contract id) generated at test-design
	// time so the test fixture isn't dependent on encoding helpers.
	cSAC    = "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4"
	gHolder = "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF"
	// A parseable classic asset_key: NewObserver validates it.
	usdcKey = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
)

func mustEdAccount(t *testing.T, gAddr string) [32]byte {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteAccountID, gAddr)
	if err != nil {
		t.Fatalf("strkey.Decode(%q): %v", gAddr, err)
	}
	var k [32]byte
	copy(k[:], raw)
	return k
}

func mustContractID(t *testing.T, cAddr string) [32]byte {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, cAddr)
	if err != nil {
		t.Fatalf("strkey.Decode(%q): %v", cAddr, err)
	}
	var k [32]byte
	copy(k[:], raw)
	return k
}

func makeBalanceKey(t *testing.T, holder string) xdr.ScVal {
	t.Helper()
	holderPK := mustEdAccount(t, holder)
	holderAID := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: (*xdr.Uint256)(&holderPK)}
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &holderAID}
	addrSV := xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}

	sym := xdr.ScSymbol("Balance")
	symSV := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}

	vec := xdr.ScVec{symSV, addrSV}
	vp := &vec
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vp}
}

func makeI128Val(amount int64) xdr.ScVal {
	return xdr.ScVal{
		Type: xdr.ScValTypeScvI128,
		I128: &xdr.Int128Parts{Hi: 0, Lo: xdr.Uint64(amount)},
	}
}

func makeBalanceMapVal(amount int64) xdr.ScVal {
	amtSV := makeI128Val(amount)
	amtSym := xdr.ScSymbol("amount")
	authSym := xdr.ScSymbol("authorized")
	clbSym := xdr.ScSymbol("clawback")
	trueB := true
	m := xdr.ScMap{
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &amtSym}, Val: amtSV},
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &authSym}, Val: xdr.ScVal{Type: xdr.ScValTypeScvBool, B: &trueB}},
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &clbSym}, Val: xdr.ScVal{Type: xdr.ScValTypeScvBool, B: &trueB}},
	}
	mp := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
}

func makeContractDataChange(t *testing.T, contractID string, key xdr.ScVal, val xdr.ScVal) xdr.LedgerEntryChange {
	t.Helper()
	cid := mustContractID(t, contractID)
	contract := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: (*xdr.ContractId)(&cid)}
	return xdr.LedgerEntryChange{
		Type: xdr.LedgerEntryChangeTypeLedgerEntryUpdated,
		Updated: &xdr.LedgerEntry{
			Data: xdr.LedgerEntryData{
				Type: xdr.LedgerEntryTypeContractData,
				ContractData: &xdr.ContractDataEntry{
					Contract:   contract,
					Key:        key,
					Durability: xdr.ContractDataDurabilityPersistent,
					Val:        val,
				},
			},
		},
	}
}

func TestNewObserver_RejectsEmpty(t *testing.T) {
	if _, err := NewObserver(nil); !errors.Is(err, ErrEmptyWrapperMap) {
		t.Errorf("nil: err=%v want ErrEmptyWrapperMap", err)
	}
	if _, err := NewObserver(map[string]string{"": usdcKey}); err == nil {
		t.Errorf("empty contract id should error")
	}
	if _, err := NewObserver(map[string]string{cSAC: ""}); err == nil {
		t.Errorf("empty asset_key should error")
	}
}

func TestObserver_MatchesWatchedSAC_I128Val(t *testing.T) {
	o, err := NewObserver(map[string]string{cSAC: usdcKey})
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	change := makeContractDataChange(t, cSAC, makeBalanceKey(t, gHolder), makeI128Val(1_000_000))
	if !o.Matches(change) {
		t.Errorf("expected match on watched SAC + i128 balance value")
	}
}

func TestObserver_MatchesWatchedSAC_MapVal(t *testing.T) {
	o, err := NewObserver(map[string]string{cSAC: usdcKey})
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	change := makeContractDataChange(t, cSAC, makeBalanceKey(t, gHolder), makeBalanceMapVal(1_000_000))
	if !o.Matches(change) {
		t.Errorf("expected match on watched SAC + BalanceValue map")
	}
}

// TestObserver_SkipsWrongKey — the contract is watched but the
// Key isn't a Balance entry (e.g. it's a metadata key). Match
// should reject.
func TestObserver_SkipsWrongKey(t *testing.T) {
	o, _ := NewObserver(map[string]string{cSAC: usdcKey})
	wrongSym := xdr.ScSymbol("Allowance")
	wrongVec := xdr.ScVec{xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &wrongSym}}
	wp := &wrongVec
	wrongKey := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &wp}
	change := makeContractDataChange(t, cSAC, wrongKey, makeI128Val(1))
	if o.Matches(change) {
		t.Errorf("expected NO match — key is Allowance, not Balance")
	}
}

func TestObserver_DecodeI128(t *testing.T) {
	o, _ := NewObserver(map[string]string{cSAC: usdcKey})
	change := makeContractDataChange(t, cSAC, makeBalanceKey(t, gHolder), makeI128Val(987_654_321))
	outs, err := o.Decode(dispatcher.LedgerEntryChangeContext{
		Ledger: 1, Change: change, ClosedAt: time.Unix(1, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	obs := outs[0].(Observation)
	if obs.AssetKey != usdcKey {
		t.Errorf("AssetKey=%q want %q", obs.AssetKey, usdcKey)
	}
	if obs.Holder != gHolder {
		t.Errorf("Holder=%q want %q", obs.Holder, gHolder)
	}
	if obs.Balance.Int64() != 987_654_321 {
		t.Errorf("Balance=%s want 987654321", obs.Balance)
	}
	if obs.IsRemoval {
		t.Errorf("IsRemoval=true on Updated change, want false")
	}
}

func TestObserver_DecodeMapVal(t *testing.T) {
	o, _ := NewObserver(map[string]string{cSAC: usdcKey})
	change := makeContractDataChange(t, cSAC, makeBalanceKey(t, gHolder), makeBalanceMapVal(555))
	outs, err := o.Decode(dispatcher.LedgerEntryChangeContext{
		Ledger: 1, Change: change,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	obs := outs[0].(Observation)
	if obs.Balance.Int64() != 555 {
		t.Errorf("Balance=%s want 555 (from BalanceValue map)", obs.Balance)
	}
}

// TestObserver_DecodeRemoved — Removed-variant SAC entries emit
// IsRemoval=true with Balance=0. Asset_key still populates from
// the operator map.
func TestObserver_DecodeRemoved(t *testing.T) {
	o, _ := NewObserver(map[string]string{cSAC: usdcKey})
	cid := mustContractID(t, cSAC)
	contract := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: (*xdr.ContractId)(&cid)}
	change := xdr.LedgerEntryChange{
		Type: xdr.LedgerEntryChangeTypeLedgerEntryRemoved,
		Removed: &xdr.LedgerKey{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.LedgerKeyContractData{
				Contract:   contract,
				Key:        makeBalanceKey(t, gHolder),
				Durability: xdr.ContractDataDurabilityPersistent,
			},
		},
	}
	if !o.Matches(change) {
		t.Fatalf("expected match on Removed SAC balance entry")
	}
	outs, err := o.Decode(dispatcher.LedgerEntryChangeContext{
		Ledger: 1, Change: change,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	obs := outs[0].(Observation)
	if !obs.IsRemoval {
		t.Errorf("IsRemoval=false on Removed change, want true")
	}
	if obs.Balance.Sign() != 0 {
		t.Errorf("Balance=%s want 0 (removed)", obs.Balance)
	}
	if obs.AssetKey != usdcKey {
		t.Errorf("AssetKey=%q want %q (from operator map)", obs.AssetKey, usdcKey)
	}
}

// TestObserver_CanonicalizesDashFormAssetKey is a
// regression test. An operator may configure a SAC wrapper's asset_key in
// the documented canonical CODE-ISSUER (dash) wire form — the same form
// [supply].watched_classic_assets uses for the trustline / claimable /
// LP observers, which canonicalize to CODE:ISSUER (colon) via
// supply.CanonicalizeWatchedClassic. If NewObserver copied
// the asset_key verbatim, the emitted Observation carried the dash
// form and the SAC-held slice never joined the same classic asset's
// colon-form supply in derivation → the asset was UNDER-reported. The
// observer must canonicalize the asset_key to the colon form the sibling
// observers produce.
func TestObserver_CanonicalizesDashFormAssetKey(t *testing.T) {
	const (
		usdcDash  = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		usdcColon = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	)
	o, err := NewObserver(map[string]string{cSAC: usdcDash})
	if err != nil {
		t.Fatalf("NewObserver with dash-form asset_key: %v", err)
	}
	change := makeContractDataChange(t, cSAC, makeBalanceKey(t, gHolder), makeI128Val(266_000_000))
	outs, err := o.Decode(dispatcher.LedgerEntryChangeContext{
		Ledger: 1, Change: change, ClosedAt: time.Unix(1, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	obs := outs[0].(Observation)
	if obs.AssetKey != usdcColon {
		t.Errorf("AssetKey=%q, want canonical colon form %q — a dash-form SAC "+
			"asset_key must canonicalize to match the classic asset's watched-set "+
			"key, else its SAC-held supply is under-reported", obs.AssetKey, usdcColon)
	}
	// The observed balance/scale must be untouched by the key fix (ADR-0003).
	if obs.Balance.Int64() != 266_000_000 {
		t.Errorf("Balance=%s, want 266000000 (canonicalization must not alter the value)", obs.Balance)
	}
}

// TestObserver_PureSEP41ContractIDPassesThrough guards the fix's
// contract_id → contract_id branch: a pure SEP-41 wrapper maps a bare
// C-strkey (not a classic CODE-ISSUER asset) and must pass through the
// canonicalizer unchanged, not be rejected as a non-classic asset.
func TestObserver_PureSEP41ContractIDPassesThrough(t *testing.T) {
	o, err := NewObserver(map[string]string{cSAC: cSAC})
	if err != nil {
		t.Fatalf("NewObserver with contract_id asset_key: %v", err)
	}
	change := makeContractDataChange(t, cSAC, makeBalanceKey(t, gHolder), makeI128Val(42))
	outs, err := o.Decode(dispatcher.LedgerEntryChangeContext{Ledger: 1, Change: change})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if obs := outs[0].(Observation); obs.AssetKey != cSAC {
		t.Errorf("AssetKey=%q, want %q (bare contract_id must pass through unchanged)", obs.AssetKey, cSAC)
	}
}

func TestObserver_RoundTripThroughDispatcher(t *testing.T) {
	o, _ := NewObserver(map[string]string{cSAC: usdcKey})
	disp := dispatcher.New()
	disp.AddEntryDecoder(o)

	change := makeContractDataChange(t, cSAC, makeBalanceKey(t, gHolder), makeI128Val(100))
	outs, err := disp.RouteEntryChange(dispatcher.LedgerEntryChangeContext{
		Ledger: 1, Change: change,
	})
	if err != nil {
		t.Fatalf("RouteEntryChange: %v", err)
	}
	if len(outs) != 1 || outs[0].EventKind() != ObservationKind {
		t.Errorf("dispatcher round-trip lost the observation")
	}
}

// TestObserver_RemovalCarriesIntraLedgerSeq: an eviction/deletion must keep
// its within-ledger position, or a same-ledger delete-then-recreate can be
// resolved to the wrong final state.
func TestObserver_RemovalCarriesIntraLedgerSeq(t *testing.T) {
	o, _ := NewObserver(map[string]string{cSAC: usdcKey})
	cid := mustContractID(t, cSAC)
	change := xdr.LedgerEntryChange{
		Type: xdr.LedgerEntryChangeTypeLedgerEntryRemoved,
		Removed: &xdr.LedgerKey{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.LedgerKeyContractData{
				Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: (*xdr.ContractId)(&cid)},
				Key:        makeBalanceKey(t, gHolder),
				Durability: xdr.ContractDataDurabilityPersistent,
			},
		},
	}
	outs, err := o.Decode(dispatcher.LedgerEntryChangeContext{Ledger: 42, Change: change, IntraLedgerSeq: 17})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	obs := outs[0].(Observation)
	if !obs.IsRemoval || obs.Balance.Sign() != 0 {
		t.Fatalf("removal = (%v, %s), want (true, 0)", obs.IsRemoval, obs.Balance)
	}
	if obs.IntraLedgerSeq != 17 || obs.Ledger != 42 {
		t.Fatalf("IntraLedgerSeq/Ledger = %d/%d, want 17/42", obs.IntraLedgerSeq, obs.Ledger)
	}
}

// TestObserver_DecodeRejectsUnwatchedContract: Decode must not trust that
// Matches ran — an unwatched contract has no asset_key, and emitting one
// with an empty key would write an orphan supply slice.
func TestObserver_DecodeRejectsUnwatchedContract(t *testing.T) {
	const cOther = "CAAQCAIBAEAQCAIBAEAQCAIBAEAQCAIBAEAQCAIBAEAQCAIBAEAQC526"
	o, _ := NewObserver(map[string]string{cSAC: usdcKey})
	change := makeContractDataChange(t, cOther, makeBalanceKey(t, gHolder), makeI128Val(1))
	if o.Matches(change) {
		t.Fatalf("Matches accepted unwatched contract %s", cOther)
	}
	outs, err := o.Decode(dispatcher.LedgerEntryChangeContext{Ledger: 1, Change: change})
	if !errors.Is(err, ErrNotSACBalance) {
		t.Fatalf("Decode(unwatched) = (%v, %v), want ErrNotSACBalance", outs, err)
	}
}

// TestObserver_DecodeRejectsForeignBalanceShapes: a balance value that is
// neither i128 nor a map with an i128 `amount` must be refused, not read
// as zero or as a different width.
func TestObserver_DecodeRejectsForeignBalanceShapes(t *testing.T) {
	o, _ := NewObserver(map[string]string{cSAC: usdcKey})
	u128 := xdr.ScVal{Type: xdr.ScValTypeScvU128, U128: &xdr.UInt128Parts{Lo: 5}}
	u64v := xdr.Uint64(5)
	u64 := xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &u64v}
	shapes := map[string]xdr.ScVal{
		"bare u128":        u128,
		"bare u64":         u64,
		"map amount u128":  balanceMap(u128, true),
		"map amount u64":   balanceMap(u64, false),
		"void (no amount)": {Type: xdr.ScValTypeScvVoid},
	}
	for name, val := range shapes {
		change := makeContractDataChange(t, cSAC, makeBalanceKey(t, gHolder), val)
		outs, err := o.Decode(dispatcher.LedgerEntryChangeContext{Ledger: 1, Change: change})
		if !errors.Is(err, ErrUnknownValShape) {
			t.Errorf("%s: Decode = (%v, %v), want ErrUnknownValShape", name, outs, err)
		}
	}
	// Every one of the above is a Matches-accepted, undecodable-value
	// shape — the exact class UnknownValShapeDrops exists to surface, since
	// without this counter a persistently-misconfigured pure-SEP-41 wrapper
	// (matches every change, decodes none) was indistinguishable from
	// occasional decode noise on any existing signal.
	if got := o.UnknownValShapeDrops(); got != len(shapes) {
		t.Errorf("UnknownValShapeDrops() = %d, want %d (one per rejected shape)", got, len(shapes))
	}
}

// TestObserver_EvictedBalanceIsObservedAsRemoval pins the eviction half of
// the Soroban state-archival lifecycle end to end through the real
// dispatcher.
//
// A SAC balance whose TTL lapses is archived at ledger close. No
// transaction touches it, so it appears in no transaction meta and the
// observer would never hear about it: the holder's last write stood as
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
