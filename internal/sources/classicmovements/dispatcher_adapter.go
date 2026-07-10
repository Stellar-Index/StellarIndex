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
// caller to resolve via a second-pass Postgres lookup
// (timescale.Store.FindClaimableBalanceCreate) — see
// TakePendingClaimableBalances / ResolvePendingClaimableBalance.
//
// The in-memory index has NO eviction — it grows with the ledger
// range one command invocation processes. A genesis-to-P23 run in a
// single invocation would accumulate on the order of the full
// CreateClaimableBalance row count (research §5: ~1.5B) before ever
// being claimed, which is not a bounded amount of memory. Operators
// backfilling Phase 3 MUST chunk `-from`/`-to` into multi-million-
// ledger invocations (same heavy-job discipline as every other
// backfill in this repo) rather than one genesis-to-P23 call — the
// Postgres fallback is what keeps chunked, resumed invocations
// correct despite each invocation starting with an empty index.
//
// Not safe for concurrent Decode calls — sequential caller only,
// matching dispatcher.Dispatcher's own "not safe for concurrent
// ProcessLedger" contract. classic-movements-backfill's loop is
// single-threaded, so this is never an issue in practice.
type Decoder struct {
	balances map[string]claimableBalanceInfo
	pending  []PendingClaimableBalanceRef
}

// claimableBalanceInfo is what a 'claimable_balance_create' movement
// contributes to the Decoder's in-run BalanceId index.
type claimableBalanceInfo struct {
	Asset     string
	Amount    canonical.Amount
	CreatedBy string
}

// NewDecoder constructs a classicmovements Decoder.
func NewDecoder() *Decoder {
	return &Decoder{balances: make(map[string]claimableBalanceInfo)}
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
func (d *Decoder) indexClaimableBalanceCreate(m Movement) {
	id, ok := m.Attributes["balance_id"].(string)
	if !ok || id == "" {
		return
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
// FREE in-memory path before falling back to Postgres. This closes
// the one same-window gap the index has: StreamClassicOps orders ops
// by (ledger_seq, tx_hash, op_index), so a claim whose tx_hash sorts
// lexicographically BEFORE its own create's tx_hash in the SAME
// window is decoded first (landing in pending) even though the
// create is indexed moments later in that same window's loop. By the
// time the whole window has been decoded, the index has caught up —
// re-checking here resolves that case for free instead of spending a
// Postgres round trip (or, worse, a false "unresolved" count) on
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
// after each streamed window and attempt a Postgres-backed second
// pass (timescale.Store.FindClaimableBalanceCreate) for each entry —
// see ResolvePendingClaimableBalance. An entry that still can't be
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
