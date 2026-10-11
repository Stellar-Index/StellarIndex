package classicmovements

import (
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// Decoder is the OpDecoder for pre-P23 classic-movement reconstruction. It is NEVER registered
// with the live dispatcher; only `stellarindex-ops classic-movements-backfill` feeds it.
//
// Stateful: a claim or clawback carries only the BalanceId, so the create's Asset/Amount come from
// the in-run balances index; claims it cannot resolve go to pending for the caller's second-pass
// ClickHouse lookup (TakePendingClaimableBalances).
//
// The index is BOUNDED at maxCBIndexEntries (FIFO): a genesis-to-P23 run would otherwise hold the
// full CreateClaimableBalance row count (~1.5B). Eviction is safe because a miss falls through to
// clickhouse.FindClaimableBalanceCreates. Chunk `-from`/`-to` to keep more claims in memory.
//
// Not safe for concurrent Decode calls (as dispatcher.Dispatcher).
type Decoder struct {
	balances map[string]claimableBalanceInfo
	// balanceOrder is a fixed-size ring buffer of balances' keys in FIFO order. "" marks an unused
	// slot; balance_id is never empty (indexClaimableBalanceCreate guards it).
	balanceOrder []string
	// balanceNext is the ring slot written next.
	balanceNext int
	pending     []PendingClaimableBalanceRef
}

// maxCBIndexEntries bounds the in-run claimable-balance-create index. At ~400 bytes per entry,
// 8,000,000 entries is ~3 GB, sized against a heavy job's MemoryMax=20G ceiling.
//
// It must be large because the claimable-balance spam era creates 2-3 million balances per
// 10k-ledger window; a smaller cap evicts a window's own creates before second-pass resolution and
// floods the slow ClickHouse fallback. A var so tests can shrink it to exercise eviction.
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

// Matches implements dispatcher.OpDecoder. True for exactly this
// package's op-only in-scope types — see matchesSupportedOp and
// recognition_test.go.
func (*Decoder) Matches(op xdr.Operation) bool {
	return matchesSupportedOp(op)
}

// Decode implements dispatcher.OpDecoder. ctx.TxSource is used directly as FromAddress because the
// caller already resolved it from stellar.operations' source_account (op override else tx source);
// ctx.OpSource is not consulted. Each 'claimable_balance_create' is indexed for later correlation.
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

// indexClaimableBalanceCreate records a create in the in-run index. A missing balance_id is a
// decoder bug, not chain data, so it is skipped rather than panicking: a later claim lands in
// pending, which is slower but never wrong.
//
// A new balance_id takes one ring slot, evicting the oldest key once the ring has wrapped. One
// already present (a retried window) is updated in place and keeps its FIFO position.
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

// ResolveBalance re-checks the in-run index so a caller draining pending after a whole window can
// skip ClickHouse. StreamClassicOps orders by (ledger_seq, tx_hash, op_index), so a claim whose
// tx_hash sorts before its own create's in the SAME ledger lands in pending; once the window is
// decoded the index has caught up.
func (d *Decoder) ResolveBalance(balanceIDHex string) (asset string, amount canonical.Amount, createdBy string, found bool) {
	info, ok := d.lookupClaimableBalance(balanceIDHex)
	if !ok {
		return "", canonical.Amount{}, "", false
	}
	return info.Asset, info.Amount, info.CreatedBy, true
}

// TakePendingClaimableBalances returns and clears the claims the in-run index could not resolve,
// for a batched ClickHouse second pass. An entry still unresolved there is a genuine
// incompleteness signal: count it, log a summary, never guess an amount.
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
