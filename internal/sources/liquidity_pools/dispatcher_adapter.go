package liquidity_pools

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sync"

	"github.com/Stellar-Index/StellarIndex/internal/supply"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// Observer is the dispatcher-facing LiquidityPoolEntry observer
// per ADR-0022 PR 4/5. Implements
// [dispatcher.LedgerEntryChangeDecoder].
//
// Watched-asset driven via the same operator config the
// trustlines + claimable observers consume.
type Observer struct {
	watched map[string]struct{}

	// mu guards preImage + preImageLedger. [dispatcher.Dispatcher]
	// documents that ProcessLedger must be serialized by the caller,
	// so this lock is uncontended in production — it is here so a
	// future concurrent caller degrades to "pairing may miss a
	// removal" rather than to a data race.
	mu sync.Mutex

	// preImage maps pool_id hex → the watched asset_keys of that pool,
	// recorded from a body-carrying change (STATE / Created / Updated /
	// Restored) seen earlier in THIS ledger's entry-change walk.
	//
	// The LedgerKey on a Removed change carries only the
	// LiquidityPoolId — not the asset pair — so a fully-withdrawn pool
	// cannot be attributed from the Removed change alone. stellar-core
	// emits the full pre-image as a LEDGER_ENTRY_STATE change
	// immediately before the LEDGER_ENTRY_REMOVED for the same entry
	// (LedgerTxn::getChanges; the SDK's
	// ingest.GetChangesFromLedgerEntryChanges relies on the same
	// adjacency), so the pair is always in hand by then.
	//
	// Scoped to one ledger: reset when a change from a different ledger
	// arrives, bounding the map by the count of watched-asset pool
	// changes in a single ledger.
	preImage       map[string][]string
	preImageLedger uint32
}

var (
	ErrEmptyWatchSet      = errors.New("liquidity_pools: cannot construct Observer with empty watched-asset list")
	ErrUnsupportedLPType  = errors.New("liquidity_pools: only ConstantProduct LP entries supported at v1")
	ErrUnsupportedLPAsset = errors.New("liquidity_pools: LP asset is not a classic credit asset")
)

func NewObserver(watched []string) (*Observer, error) {
	if len(watched) == 0 {
		return nil, ErrEmptyWatchSet
	}
	watched, err := supply.CanonicalizeWatchedClassic(watched)
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(watched))
	for _, k := range watched {
		if k == "" {
			return nil, errors.New("liquidity_pools: empty asset_key in watched list")
		}
		set[k] = struct{}{}
	}
	return &Observer{watched: set}, nil
}

func (*Observer) Name() string { return SourceName }

// Matches returns true when:
//
//  1. The change is State/Created/Updated/Restored on a
//     LiquidityPool entry, AND
//  2. The pool is ConstantProduct (only LP variant today), AND
//  3. At least one side of the asset pair is in the watched set.
//
// For Removed variants (the pool's last liquidity withdrawn) it
// returns true only when this ledger's pre-image memo already knows
// the pool's watched sides — same handling as claimable_balances.
func (o *Observer) Matches(change xdr.LedgerEntryChange) bool {
	if change.Type == xdr.LedgerEntryChangeTypeLedgerEntryRemoved {
		poolID, ok := removedPoolID(change)
		if !ok {
			return false
		}
		return o.hasPreImage(poolID)
	}
	lp, ok := lpFromChange(change)
	if !ok {
		return false
	}
	if lp.Body.Type != xdr.LiquidityPoolTypeLiquidityPoolConstantProduct {
		return false
	}
	cp := lp.Body.ConstantProduct
	if cp == nil {
		return false
	}
	for _, a := range []xdr.Asset{cp.Params.AssetA, cp.Params.AssetB} {
		ak, err := assetKeyFromAsset(a)
		if err != nil {
			continue
		}
		if _, watched := o.watched[ak]; watched {
			return true
		}
	}
	return false
}

// Decode implements [dispatcher.LedgerEntryChangeDecoder]. Emits
// one Observation per asset side that's in the watched set —
// a pool with both sides watched produces two Observations; one
// side watched produces one; neither (Match would have skipped).
//
// A Removed change (the pool's last liquidity withdrawn, entry
// deleted) emits one removal Observation per watched side — Balance
// zero, IsRemoval true — so the served aggregate stops counting the
// vanished reserves. The removal is an absorbing STATE, not a delta,
// so re-ingesting a ledger rewrites the identical rows rather than
// double-subtracting.
//
// A State change carries only the PRE-image; it feeds the memo and
// emits nothing (emitting would persist a stale reserve).
func (o *Observer) Decode(ctx dispatcher.LedgerEntryChangeContext) ([]consumer.Event, error) {
	if ctx.Change.Type == xdr.LedgerEntryChangeTypeLedgerEntryRemoved {
		poolID, ok := removedPoolID(ctx.Change)
		if !ok {
			return nil, fmt.Errorf("%w: removed change carries no LP LedgerKey", ErrUnsupportedLPType)
		}
		sides, known := o.lookupPreImage(ctx.Ledger, poolID)
		if !known {
			// Unwatched pool, or the pre-image STATE never reached us.
			return nil, nil
		}
		outs := make([]consumer.Event, 0, len(sides))
		for _, ak := range sides {
			outs = append(outs, Observation{
				PoolID:         poolID,
				AssetKey:       ak,
				Ledger:         ctx.Ledger,
				ObservedAt:     ctx.ClosedAt,
				Balance:        big.NewInt(0),
				IsRemoval:      true,
				IntraLedgerSeq: ctx.IntraLedgerSeq,
			})
		}
		return outs, nil
	}

	lp, ok := lpFromChange(ctx.Change)
	if !ok || lp.Body.ConstantProduct == nil {
		return nil, ErrUnsupportedLPType
	}
	cp := lp.Body.ConstantProduct
	poolID := hex.EncodeToString(lp.LiquidityPoolId[:])

	var (
		outs         []consumer.Event
		watchedSides []string
	)
	for _, side := range []struct {
		asset   xdr.Asset
		reserve xdr.Int64
	}{
		{cp.Params.AssetA, cp.ReserveA},
		{cp.Params.AssetB, cp.ReserveB},
	} {
		ak, err := assetKeyFromAsset(side.asset)
		if err != nil {
			continue // native + unsupported variants — skip silently
		}
		if _, watched := o.watched[ak]; !watched {
			continue
		}
		watchedSides = append(watchedSides, ak)
		outs = append(outs, Observation{
			PoolID:         poolID,
			AssetKey:       ak,
			Ledger:         ctx.Ledger,
			ObservedAt:     ctx.ClosedAt,
			Balance:        big.NewInt(int64(side.reserve)),
			IntraLedgerSeq: ctx.IntraLedgerSeq,
		})
	}
	// Remember the watched sides for the removal that may follow in
	// this ledger.
	o.rememberPreImage(ctx.Ledger, poolID, watchedSides)
	if ctx.Change.Type == xdr.LedgerEntryChangeTypeLedgerEntryState {
		return nil, nil
	}
	return outs, nil
}

// rememberPreImage records pool_id → watched asset_keys for the
// current ledger, resetting the memo when the walk moves to a new
// ledger. A pool with no watched side is not recorded.
func (o *Observer) rememberPreImage(ledger uint32, poolID string, sides []string) {
	if len(sides) == 0 {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.preImage == nil || o.preImageLedger != ledger {
		o.preImage = make(map[string][]string)
		o.preImageLedger = ledger
	}
	o.preImage[poolID] = sides
}

// lookupPreImage returns the watched asset_keys recorded for `poolID`
// earlier in THIS ledger's walk. A memo left over from a previous
// ledger never answers: stellar-core emits the pre-image STATE in the
// same change list as the REMOVED it belongs to (the SDK's reader
// asserts that adjacency outright), so a cross-ledger hit would mean
// the meta broke that invariant — in which case skipping is the honest
// answer rather than attributing a removal from stale state.
func (o *Observer) lookupPreImage(ledger uint32, poolID string) ([]string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.preImageLedger != ledger {
		return nil, false
	}
	sides, ok := o.preImage[poolID]
	return sides, ok
}

// hasPreImage is the Match-time pre-filter: does the memo know this
// pool at all? Matches gets no [dispatcher.LedgerEntryChangeContext],
// so the ledger-scoped check is Decode's job — per the decoder contract,
// Matches is the cheap filter and Decode is authoritative.
func (o *Observer) hasPreImage(poolID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.preImage[poolID]
	return ok
}

func lpFromChange(change xdr.LedgerEntryChange) (*xdr.LiquidityPoolEntry, bool) {
	var entry *xdr.LedgerEntry
	switch change.Type {
	case xdr.LedgerEntryChangeTypeLedgerEntryCreated:
		entry = change.Created
	case xdr.LedgerEntryChangeTypeLedgerEntryUpdated:
		entry = change.Updated
	case xdr.LedgerEntryChangeTypeLedgerEntryRestored:
		entry = change.Restored
	case xdr.LedgerEntryChangeTypeLedgerEntryState:
		entry = change.State
	default:
		return nil, false
	}
	if entry == nil || entry.Data.Type != xdr.LedgerEntryTypeLiquidityPool {
		return nil, false
	}
	return entry.Data.LiquidityPool, entry.Data.LiquidityPool != nil
}

// removedPoolID pulls the pool_id hex off a Removed change's
// LedgerKey. Returns ok=false when the change is not an LP removal
// (or the key is malformed). The asset pair is NOT recoverable here,
// which is why the observer pairs this with the pre-image memo.
func removedPoolID(change xdr.LedgerEntryChange) (string, bool) {
	key := change.Removed
	if key == nil || key.Type != xdr.LedgerEntryTypeLiquidityPool || key.LiquidityPool == nil {
		return "", false
	}
	return hex.EncodeToString(key.LiquidityPool.LiquidityPoolId[:]), true
}

func assetKeyFromAsset(a xdr.Asset) (string, error) {
	switch a.Type {
	case xdr.AssetTypeAssetTypeCreditAlphanum4:
		a4 := a.AlphaNum4
		if a4 == nil {
			return "", errors.New("liquidity_pools: nil AlphaNum4")
		}
		code := trimTrailingNulls(a4.AssetCode[:])
		pk, ok := a4.Issuer.GetEd25519()
		if !ok {
			return "", fmt.Errorf("liquidity_pools: alphanum4 issuer not Ed25519 (type=%d)", a4.Issuer.Type)
		}
		issuer, err := strkey.Encode(strkey.VersionByteAccountID, pk[:])
		if err != nil {
			return "", fmt.Errorf("liquidity_pools: alphanum4 issuer: %w", err)
		}
		return code + ":" + issuer, nil
	case xdr.AssetTypeAssetTypeCreditAlphanum12:
		a12 := a.AlphaNum12
		if a12 == nil {
			return "", errors.New("liquidity_pools: nil AlphaNum12")
		}
		code := trimTrailingNulls(a12.AssetCode[:])
		pk, ok := a12.Issuer.GetEd25519()
		if !ok {
			return "", fmt.Errorf("liquidity_pools: alphanum12 issuer not Ed25519 (type=%d)", a12.Issuer.Type)
		}
		issuer, err := strkey.Encode(strkey.VersionByteAccountID, pk[:])
		if err != nil {
			return "", fmt.Errorf("liquidity_pools: alphanum12 issuer: %w", err)
		}
		return code + ":" + issuer, nil
	}
	return "", ErrUnsupportedLPAsset
}

func trimTrailingNulls(b []byte) string {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] != 0 {
			return string(b[:i+1])
		}
	}
	return ""
}

var _ dispatcher.LedgerEntryChangeDecoder = (*Observer)(nil)
