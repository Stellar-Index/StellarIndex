package v1

import (
	"context"
	"time"

	blend_backstop "github.com/Stellar-Index/StellarIndex/internal/sources/blend_backstop"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
)

// ledgerCloseTimeReader resolves a writer cursor's ledger to the close time the event tables are keyed on.
type ledgerCloseTimeReader interface {
	CloseTimeForLedger(ctx context.Context, seq uint32) (time.Time, bool, error)
}

// emissionsBounds is each stream group's window: the pool streams (reserves, pool_claims) end where the protocol's own
// writer has committed, the backstop streams where the backstop's has. reason names a cut short of the lake's end.
type emissionsBounds struct {
	pool, backstop             emissionsHistoryWindow
	poolReason, backstopReason string
}

// emissionsWriterBounds cuts each group at the earlier of the lake-bounded end and its writer's committed ledger: the
// events live in Postgres tables the projector writes, so a bucket the lake has passed but the writer has not would read
// as "nothing happened", and a schedule run past a missed update would extend the old rate.
func (s *Server) emissionsWriterBounds(ctx context.Context, win emissionsHistoryWindow, isBLNT, isV1 bool, flags *Flags) emissionsBounds {
	poolSrc, bstopSrc := "blend", blend_backstop.SourceName
	if isBLNT {
		poolSrc, bstopSrc = blnt.SourceName, blnt.SourceName
	}
	b := emissionsBounds{pool: win, backstop: win}
	b.pool.end, b.poolReason = s.emissionsWriterEnd(ctx, poolSrc, win, flags)
	if !isV1 {
		b.backstop.end, b.backstopReason = s.emissionsWriterEnd(ctx, bstopSrc, win, flags)
	}
	return b
}

// emissionsWriterEnd is the exclusive end the source's slowest writer has committed through: one second past its ledger's
// close time, since close times strictly increase. An unreadable cursor or close time serves the lake-bounded end flagged.
func (s *Server) emissionsWriterEnd(ctx context.Context, source string, win emissionsHistoryWindow, flags *Flags) (time.Time, string) {
	through := s.badDebtWrittenThrough(ctx, source, flags)
	lr, ok := s.Explorer.(ledgerCloseTimeReader)
	if through == 0 || s.Explorer == nil || !ok {
		return win.end, emisHistWriterUnread
	}
	closed, found, err := lr.CloseTimeForLedger(ctx, through)
	if err != nil || !found {
		if err != nil {
			s.logger.Warn("lending emissions history: writer close time read failed", "source", source, "ledger", through, "err", err)
			flags.Degraded = true
		}
		return win.end, emisHistWriterUnread
	}
	end := closed.UTC().Truncate(time.Second).Add(time.Second)
	if !end.Before(win.end) {
		return win.end, ""
	}
	return maxTime(end, win.from), emisHistWriterBehind
}

// apply names each cut, sets each stream's served_through and the view's (the earliest available stream's). A schedule
// whose writer cannot be placed is withheld: past an unknown cursor the rate is unknown, and extending it can overstate.
func (b emissionsBounds) apply(v *LendingEmissionsHistoryView) {
	if b.poolReason == emisHistWriterUnread && v.Reserves.Available {
		v.Reserves.lendingEmissionStreamHead = unavailable(emisHistWriterUnread)
		v.Reserves.Streams = []LendingEmissionScheduleLine{}
	}
	type stream struct {
		head   *lendingEmissionStreamHead
		name   string
		win    emissionsHistoryWindow
		reason string
	}
	streams := []stream{
		{&v.Reserves.lendingEmissionStreamHead, emisStreamReserves, b.pool, b.poolReason},
		{&v.PoolClaims.lendingEmissionStreamHead, emisStreamPoolClaims, b.pool, b.poolReason},
		{&v.BackstopAlloc.lendingEmissionStreamHead, emisStreamBackstopAlloc, b.backstop, b.backstopReason},
		{&v.BackstopClaims.lendingEmissionStreamHead, emisStreamBackstopClaims, b.backstop, b.backstopReason},
	}
	served := time.Time(v.ServedThrough)
	for _, st := range streams {
		if !st.head.Available {
			continue
		}
		t := WireTime(st.win.end)
		st.head.ServedThrough = &t
		served = minTime(served, st.win.end)
		if st.reason != "" {
			v.exclude(st.name, st.reason, nil, nil)
		}
	}
	v.ServedThrough = WireTime(served)
}
