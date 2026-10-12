package classicmovements

import (
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// Decoder is the OpDecoder for pre-P23 classic-movement reconstruction, NEVER registered with the live dispatcher (only `stellarindex-ops classic-movements-backfill` feeds it); it is stateful and not safe for concurrent Decode: claims resolve the create's Asset/Amount via a FIFO-bounded (maxCBIndexEntries) in-run balances index, and misses go to pending for the ClickHouse second pass (TakePendingClaimableBalances).
type Decoder struct {
	balances map[string]claimableBalanceInfo
	// balanceOrder is a fixed-size ring buffer of balances' keys in FIFO order. "" marks an unused
	// slot; balance_id is never empty (indexClaimableBalanceCreate guards it).
	balanceOrder []string
	// balanceNext is the ring slot written next.
	balanceNext int
	pending     []PendingClaimableBalanceRef
}

// maxCBIndexEntries bounds the in-run claimable-balance-create index (~400 B per entry, so 8M is ~3 GB against a heavy job's
// MemoryMax=20G). It must be large: the spam era creates 2-3M balances per 10k-ledger window, and a smaller cap evicts a window's own
// creates and floods the slow ClickHouse fallback. A var so tests can shrink it.
var maxCBIndexEntries = 8_000_000

// claimableBalanceInfo is what a 'claimable_balance_create' movement
// contributes to the Decoder's in-run BalanceId index.
type claimableBalanceInfo struct {
	Asset     string
	Amount    canonical.Amount
	CreatedBy string
}

// NewDecoder constructs a classicmovements Decoder.
func NewDecoder() *Decoder {
	return &Decoder{
		balances:     make(map[string]claimableBalanceInfo),
		balanceOrder: make([]string, maxCBIndexEntries),
	}
}

// Name implements dispatcher.OpDecoder.
func (*Decoder) Name() string { return SourceName }

// Matches implements dispatcher.OpDecoder: true for exactly this package's op-only in-scope types (see recognition_test.go).
func (*Decoder) Matches(op xdr.Operation) bool {
	return matchesSupportedOp(op)
}

// Decode implements dispatcher.OpDecoder; ctx.TxSource is used directly as FromAddress because the caller already resolved it from stellar.operations' source_account, ctx.OpSource is not consulted, and each 'claimable_balance_create' is indexed for later correlation.
func (d *Decoder) Decode(ctx dispatcher.OpContext) ([]consumer.Event, error) {
	movements, err := d.decodeOp(ctx.Ledger, ctx.ClosedAt, ctx.TxHash, uint32(ctx.OpIndex), ctx.TxSource, ctx.Op, ctx.OpResult) //nolint:gosec // OpIndex is a non-negative XDR index; widening int->uint32 is safe for real ledger data.
	if err != nil {
		return nil, err
	}
	out := make([]consumer.Event, 0, len(movements))
	for _, m := range movements {
		if m.Kind == KindClaimableBalanceCreate {
			d.indexClaimableBalanceCreate(m)
		}
		out = append(out, MovementEvent{Movement: m})
	}
	return out, nil
}

// indexClaimableBalanceCreate records a create in the in-run index. A missing balance_id is a decoder bug, skipped rather than panicking
// (a later claim lands in pending: slower, never wrong). A new id takes one ring slot, evicting the oldest once wrapped; a present one
// (retried window) updates in place.
func (d *Decoder) indexClaimableBalanceCreate(m Movement) {
	id, ok := m.Attributes["balance_id"].(string)
	if !ok || id == "" {
		return
	}
	if _, exists := d.balances[id]; !exists && len(d.balanceOrder) > 0 {
		if evicted := d.balanceOrder[d.balanceNext]; evicted != "" {
			delete(d.balances, evicted)
		}
		d.balanceOrder[d.balanceNext] = id
		d.balanceNext = (d.balanceNext + 1) % len(d.balanceOrder)
	}
	d.balances[id] = claimableBalanceInfo{
		Asset:     m.Asset,
		Amount:    m.Amount,
		CreatedBy: m.FromAddress,
	}
}

// lookupClaimableBalance resolves a balance_id against the in-run
// index only (no I/O) — the hot path for a claim/clawback whose
// create was observed earlier in this same invocation.
func (d *Decoder) lookupClaimableBalance(balanceIDHex string) (claimableBalanceInfo, bool) {
	info, ok := d.balances[balanceIDHex]
	return info, ok
}

// recordPending appends a claim/clawback this Decoder's in-run index
// couldn't resolve — see TakePendingClaimableBalances.
func (d *Decoder) recordPending(ref PendingClaimableBalanceRef) {
	d.pending = append(d.pending, ref)
}

// ResolveBalance re-checks the in-run index so a caller draining pending after a whole window can skip ClickHouse; a claim whose tx_hash sorts before its create's in the SAME ledger lands in pending until the window is decoded.
func (d *Decoder) ResolveBalance(balanceIDHex string) (asset string, amount canonical.Amount, createdBy string, found bool) {
	info, ok := d.lookupClaimableBalance(balanceIDHex)
	if !ok {
		return "", canonical.Amount{}, "", false
	}
	return info.Asset, info.Amount, info.CreatedBy, true
}

// TakePendingClaimableBalances returns and clears the claims the in-run index could not resolve, for a batched ClickHouse second pass; an entry still unresolved there is a genuine incompleteness signal: count it, log it, never guess an amount.
func (d *Decoder) TakePendingClaimableBalances() []PendingClaimableBalanceRef {
	out := d.pending
	d.pending = nil
	return out
}

// Compile-time checks — catches interface drift at build time.
var (
	_ dispatcher.OpDecoder = (*Decoder)(nil)
	_ consumer.Event       = MovementEvent{}
)
