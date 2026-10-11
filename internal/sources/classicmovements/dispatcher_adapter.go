package classicmovements

import (
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// Decoder is the OpDecoder for pre-P23 classic-movement reconstruction. It is NEVER registered
// with the live dispatcher; only `stellarindex-ops classic-movements-backfill` feeds
// clickhouse.ClassicOp values through Decode as a dispatcher.OpContext, as ch-rebuild's SDEX pass
// does.
//
// Stateful: claiming or clawing back a claimable balance needs that create's Asset/Amount, which
// neither op carries (only the BalanceId). balances is an in-RUN index filled as Decode sees
// 'claimable_balance_create' movements; pending collects claims it cannot resolve (create outside
// this run's range or in a not-yet-visited window) for the caller's second-pass ClickHouse lookup,
// via TakePendingClaimableBalances / ResolvePendingClaimableBalance.
//
// The index is BOUNDED at maxCBIndexEntries (FIFO, oldest create evicted first): a genesis-to-P23
// run would otherwise hold about the full CreateClaimableBalance row count (~1.5B) before any
// claim. Eviction is safe: a miss falls through to clickhouse.FindClaimableBalanceCreates, the
// batched second pass. Chunk `-from`/`-to` to keep more claims on the in-memory path.
//
// Not safe for concurrent Decode calls (as dispatcher.Dispatcher).
type Decoder struct {
	balances map[string]claimableBalanceInfo
	// balanceOrder is a fixed-size ring buffer (len == maxCBIndexEntries) recording FIFO insertion
	// order of balances' keys. "" marks an unused slot; balance_id is never empty
	// (indexClaimableBalanceCreate guards it), so it is a safe sentinel.
	balanceOrder []string
	// balanceNext is the ring slot indexClaimableBalanceCreate writes
	// to next.
	balanceNext int
	pending     []PendingClaimableBalanceRef
}

// maxCBIndexEntries bounds the Decoder's in-run claimable-balance-create index. At ~400 bytes per
// entry, 8,000,000 entries is ~3 GB, sized against a heavy job's MemoryMax=20G ceiling.
//
// It must be large because the claimable-balance spam era (ledgers ~49M+) creates 2-3 million
// balances per 10k-ledger window. A smaller cap evicts a window's own creates before its
// second-pass resolution, dumping 600k+ refs per window onto the ClickHouse fallback (~2.5 min
// per scan over 695M cb-create rows). 8M keeps ~3 spam windows of locality, so memory resolves
// nearly all claims and the CH fallback stays rare. A var so tests can shrink it to exercise
// eviction without allocating millions of ring slots.
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

// Decode implements dispatcher.OpDecoder. ctx.TxSource is used directly as FromAddress; the caller
// populates it from stellar.operations' resolved source_account (op override else tx source), as
// ch-rebuild's SDEX pass does. ctx.OpSource is not consulted because there is no unresolved
// source to fall back from.
//
// Every emitted 'claimable_balance_create' movement is indexed by balance_id for later
// claim/clawback correlation within this Decoder's lifetime.
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

// indexClaimableBalanceCreate records a decoded 'claimable_balance_create' movement in the in-run
// index. A missing balance_id would be a bug in decodeCreateClaimableBalance, not bad chain data,
// so it is skipped rather than panicking: a later claim just lands in pending, which is slower but
// never wrong.
//
// A new balance_id consumes one ring slot, first evicting the oldest key if the ring has wrapped
// (see maxCBIndexEntries). A balance_id already present (a retried window re-decoding the op) is
// updated in place and does not consume a slot or move in FIFO order.
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

// ResolveBalance re-checks the in-run index for balanceIDHex so a caller draining
// TakePendingClaimableBalances after a whole window can retry the free in-memory path before
// ClickHouse. It closes the same-window gap: StreamClassicOps orders ops by (ledger_seq, tx_hash,
// op_index), so a claim whose tx_hash sorts before its own create's in the SAME ledger is decoded
// first and lands in pending. Once the window is decoded the index has caught up, avoiding a
// ClickHouse round trip or a false "unresolved" count.
func (d *Decoder) ResolveBalance(balanceIDHex string) (asset string, amount canonical.Amount, createdBy string, found bool) {
	info, ok := d.lookupClaimableBalance(balanceIDHex)
	if !ok {
		return "", canonical.Amount{}, "", false
	}
	return info.Asset, info.Amount, info.CreatedBy, true
}

// TakePendingClaimableBalances returns every claim/clawback the in-run index could not resolve
// since the last call, and clears the buffer. The caller drains it after each streamed window for
// a batched ClickHouse second pass (clickhouse.FindClaimableBalanceCreates). An entry still
// unresolved there is a genuine incompleteness signal: count it, log a summary, never guess an
// amount.
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
