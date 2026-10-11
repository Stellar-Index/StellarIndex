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

// Observer is the dispatcher-facing LiquidityPoolEntry observer (ADR-0022), a
// [dispatcher.LedgerEntryChangeDecoder] driven by the same watched-asset config as the trustlines
// and claimable observers.
type Observer struct {
	watched map[string]struct{}

	// mu guards preImage + preImageLedger. ProcessLedger is serialized by the caller, so this is
	// uncontended; it makes a concurrent caller miss a pairing rather than race.
	mu sync.Mutex

	// preImage maps pool_id hex → the watched asset_keys of that pool,
	// recorded from a body-carrying change seen earlier in THIS ledger's entry-change walk.
	//
	// A Removed change's LedgerKey carries only the LiquidityPoolId, not the asset pair.
	// stellar-core emits the full pre-image as a LEDGER_ENTRY_STATE change immediately before the
	// REMOVED for the same entry (the SDK's change reader relies on the same adjacency), so the
	// pair is known by then.
	//
	// Scoped to one ledger: reset when a change from a different ledger arrives, which bounds the
	// map.
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

// Matches returns true for a body-carrying change on a ConstantProduct pool (the only LP variant)
// with at least one side in the watched set. A Removed variant matches only when the pre-image
// memo already knows the pool's watched sides, as in claimable_balances.
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

// Decode implements [dispatcher.LedgerEntryChangeDecoder]. Emits one Observation per watched
// asset side.
//
// A Removed change (pool deleted) emits one removal Observation per watched side (Balance zero,
// IsRemoval true) so the served aggregate stops counting the vanished reserves. The removal is an
// absorbing state, not a delta, so re-ingesting a ledger rewrites the identical rows rather than
// double-subtracting.
//
// A State change carries only the pre-image; it feeds the memo and emits nothing, since emitting
// would persist a stale reserve.
func (o *Observer) Decode(ctx dispatcher.LedgerEntryChangeContext) ([]consumer.Event, error) {
	if ctx.Change.Type == xdr.LedgerEntryChangeTypeLedgerEntryRemoved {
		poolID, ok := removedPoolID(ctx.Change)
		if !ok {
			return nil, fmt.Errorf("%w: removed change carries no LP LedgerKey", ErrUnsupportedLPType)
		}
		sides, known := o.lookupPreImage(ctx.Ledger, poolID)
		if !known {
			// hasPreImage does not apply lookupPreImage's ledger guard, so the memo entry is from
			// a PREVIOUS ledger and nothing is attributable.
			//
			// Error rather than skip silently: a dropped removal leaves the pool's reserves in
			// SumLPReservesAtOrBefore permanently, over-reporting supply. The error lands on the
			// per-source decode-error counter.
			return nil, fmt.Errorf(
				"%w: removed pool %s has no pre-image in ledger %d's memo — "+
					"a watched pool's reserves cannot be attributed, so they "+
					"will stay in the served total",
				ErrUnsupportedLPType, poolID, ctx.Ledger)
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

// lookupPreImage returns the watched asset_keys recorded for poolID earlier in THIS ledger's
// walk. A memo from a previous ledger never answers: the pre-image STATE shares a change list
// with its REMOVED, so a cross-ledger hit means the meta broke that invariant, and skipping beats
// attributing a removal from stale state.
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
