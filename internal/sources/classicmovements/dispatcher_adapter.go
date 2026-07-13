package classicmovements

import (
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/consumer"
	"github.com/StellarIndex/stellar-index/internal/dispatcher"
)

// Decoder is the OpDecoder for pre-P23 classic-movement
// reconstruction (ADR-0047 D2). It mirrors sdex.Decoder's shape
// (SDEX is the established precedent for a classic-op decoder that
// lives OUTSIDE the projector) but, unlike SDEX, is NEVER registered
// with the live dispatcher — see the package doc for why. It is
// wired only into `stellarindex-ops classic-movements-backfill`,
// which streams clickhouse.ClassicOp values (via StreamClassicOps)
// and feeds them through Decode as a dispatcher.OpContext, exactly
// as ch-rebuild's SDEX pass does with clickhouse.SDEXOp.
//
// Stateful since Phase 3: claiming or clawing back a
// CreateClaimableBalance needs that create's Asset/Amount, which
// neither op carries directly (only the BalanceId — research §2's
// "b+own-index" path). balances is an in-RUN index (populated as
// this Decoder's own Decode calls observe 'claimable_balance_create'
// movements — see decodeOp's caller in Decode below) that resolves
// same-run claims/clawbacks for free; pending collects the ones this
// index can't resolve (create out of this run's range, or landed in
// a not-yet-visited window — see doc.go's ordering caveat) for the
// caller to resolve via a second-pass ClickHouse lookup (ADR-0048 D2;
// previously Postgres) — see
// TakePendingClaimableBalances / ResolvePendingClaimableBalance.
//
// The in-memory index is BOUNDED at maxCBIndexEntries (FIFO eviction,
// oldest create evicted first) — a genesis-to-P23 run in a single
// invocation would otherwise accumulate on the order of the full
// CreateClaimableBalance row count (research §5: ~1.5B) before ever
// being claimed, which is what drove an earlier OOM. Eviction is safe
// because a miss here is not data loss: ResolveBalance failing just
// means the pending entry falls through to
// clickhouse.FindClaimableBalanceCreates, classic-movements-backfill's
// ClickHouse-backed second pass (batched across a whole window's
// misses, not one query per ref), which resolves any create this run
// has itself already written — the same fallback path used for
// creates outside this run's range entirely. Operators backfilling
// Phase 3 should still chunk `-from`/`-to` into multi-million-ledger
// invocations (same heavy-job discipline as every other backfill in
// this repo): the bound protects against OOM, but a smaller working
// set keeps more claims resolving from the free in-memory path
// instead of paying a ClickHouse round trip.
//
// Not safe for concurrent Decode calls — sequential caller only,
// matching dispatcher.Dispatcher's own "not safe for concurrent
// ProcessLedger" contract. classic-movements-backfill's loop is
// single-threaded, so this is never an issue in practice.
type Decoder struct {
	balances map[string]claimableBalanceInfo
	// balanceOrder is a fixed-size ring buffer (len ==
	// maxCBIndexEntries at construction time) recording FIFO
	// insertion order of balances' keys — see indexClaimableBalanceCreate.
	// A slot holding "" is unused (never written yet); balance_id is
	// never empty (guarded in indexClaimableBalanceCreate), so "" is a
	// safe empty sentinel.
	balanceOrder []string
	// balanceNext is the ring slot indexClaimableBalanceCreate writes
	// to next.
	balanceNext int
	pending     []PendingClaimableBalanceRef
}

// maxCBIndexEntries bounds the Decoder's in-run claimable-balance-
// create index (balances / balanceOrder above). Each entry is small —
// a hex balance_id key plus a claimableBalanceInfo of two short
// strings and a *big.Int amount, ~400 bytes all-in with map overhead —
// so 8,000,000 entries is ~3 GB, sized against a heavy job's
// MemoryMax=20G ceiling (CLAUDE.md "Heavy one-shot jobs on r1").
//
// Why 8M and not less: the 2021 claimable-balance spam era (ledgers
// ~49M+) creates 2–3 MILLION balances per 10k-ledger backfill window.
// The original 2M cap evicted a window's own creates before its
// second-pass resolution ran (2026-07-13: "0 resolved (index)" with
// 600k+ refs/window dumped onto the ClickHouse fallback, each such
// scan ~2.5 min over 695M cb-create rows). 8M keeps ~3 spam windows
// of locality — spam claims land seconds-to-minutes after their
// create, so in-memory resolution does almost all the work and the
// CH fallback stays the rare path it was designed to be. A var, not
// a const, so tests can shrink it to exercise eviction without
// allocating millions of ring slots.
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

// Decode implements dispatcher.OpDecoder. ctx.TxSource is used
// directly as the movement's FromAddress — the caller (StreamClassicOps'
// consumer) is expected to populate it from stellar.operations'
// already-resolved source_account column (op override else tx
// source), the same convention ch-rebuild's SDEX pass uses. ctx.OpSource
// is intentionally NOT consulted (unlike sdex.Decoder.Decode) since
// there is no second, unresolved source to fall back from.
//
// Every emitted 'claimable_balance_create' movement is indexed by
// its balance_id for later claim/clawback correlation within this
// same Decoder instance's lifetime (see the type doc).
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

// indexClaimableBalanceCreate records a just-decoded
// 'claimable_balance_create' movement into the in-run BalanceId
// index. balance_id is always present in Attributes for this kind
// (decodeCreateClaimableBalance's contract) — a missing/malformed
// value here would indicate a bug in that function, not bad chain
// data, so it's silently skipped rather than panicking: worst case,
// a later claim/clawback falls through to the pending list instead
// of resolving from memory, which is still correct (just slower),
// never wrong.
//
// A genuinely new balance_id consumes one ring slot: if the ring has
// wrapped all the way around (balanceOrder[d.balanceNext] already
// holds a live key), that oldest key is evicted from balances first —
// see maxCBIndexEntries' doc comment for why eviction is safe. A
// balance_id already present in the index (re-decoding the same op,
// e.g. on a retried window) is an in-place value update and does NOT
// consume a new ring slot or move in FIFO order.
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

// ResolveBalance re-checks the in-run BalanceId index for
// balanceIDHex — exported so a caller draining
// TakePendingClaimableBalances after a whole window can retry the
// FREE in-memory path before falling back to ClickHouse. This closes
// the one same-window gap the index has: StreamClassicOps orders ops
// by (ledger_seq, tx_hash, op_index), so a claim whose tx_hash sorts
// lexicographically BEFORE its own create's tx_hash in the SAME
// window is decoded first (landing in pending) even though the
// create is indexed moments later in that same window's loop. By the
// time the whole window has been decoded, the index has caught up —
// re-checking here resolves that case for free instead of spending a
// ClickHouse round trip (or, worse, a false "unresolved" count) on
// same-window data that was there all along.
func (d *Decoder) ResolveBalance(balanceIDHex string) (asset string, amount canonical.Amount, createdBy string, found bool) {
	info, ok := d.lookupClaimableBalance(balanceIDHex)
	if !ok {
		return "", canonical.Amount{}, "", false
	}
	return info.Asset, info.Amount, info.CreatedBy, true
}

// TakePendingClaimableBalances returns every claim/clawback this
// Decoder's in-run index has been unable to resolve since the last
// call (or since construction), and clears its internal buffer. The
// caller (classic-movements-backfill) is expected to drain this
// after each streamed window and attempt a ClickHouse-backed second
// pass (clickhouse.FindClaimableBalanceCreates, batched across the
// whole window's misses — ADR-0048 D2; previously Postgres) for each
// entry — see ResolvePendingClaimableBalance. An entry that still can't be
// resolved there is a genuine ADR-0047 D4 recognizable-incompleteness
// signal: count it, log a summary, never guess an amount.
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
