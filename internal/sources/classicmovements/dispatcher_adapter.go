package classicmovements

import (
	"github.com/stellar/go-stellar-sdk/xdr"

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
// Stateless — every op is self-contained; Phase 1's two kinds never
// need cross-op correlation (unlike SDEX's claim atoms or Phase 3's
// future claimable-balance BalanceId correlation).
type Decoder struct{}

// NewDecoder constructs a classicmovements Decoder.
func NewDecoder() *Decoder { return &Decoder{} }

// Name implements dispatcher.OpDecoder.
func (*Decoder) Name() string { return SourceName }

// Matches implements dispatcher.OpDecoder. True for exactly Phase
// 1's in-scope op types (CreateAccount, Payment) — see
// matchesPhase1Op and recognition_test.go.
func (*Decoder) Matches(op xdr.Operation) bool {
	return matchesPhase1Op(op)
}

// Decode implements dispatcher.OpDecoder. ctx.TxSource is used
// directly as the movement's FromAddress — the caller (StreamClassicOps'
// consumer) is expected to populate it from stellar.operations'
// already-resolved source_account column (op override else tx
// source), the same convention ch-rebuild's SDEX pass uses. ctx.OpSource
// is intentionally NOT consulted (unlike sdex.Decoder.Decode) since
// there is no second, unresolved source to fall back from.
func (*Decoder) Decode(ctx dispatcher.OpContext) ([]consumer.Event, error) {
	movements, err := decodeOp(ctx.Ledger, ctx.ClosedAt, ctx.TxHash, uint32(ctx.OpIndex), ctx.TxSource, ctx.Op, ctx.OpResult) //nolint:gosec // OpIndex is a non-negative XDR index; widening int->uint32 is safe for real ledger data.
	if err != nil {
		return nil, err
	}
	out := make([]consumer.Event, 0, len(movements))
	for _, m := range movements {
		out = append(out, MovementEvent{Movement: m})
	}
	return out, nil
}

// Compile-time checks — catches interface drift at build time.
var (
	_ dispatcher.OpDecoder = (*Decoder)(nil)
	_ consumer.Event       = MovementEvent{}
)
