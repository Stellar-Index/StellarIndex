package v1

import (
	"context"
	"io"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// deadlineMarketSourceReader fails the per-source aggregate the way the
// handler's own budget does: the driver's error WRAPPING
// context.DeadlineExceeded, arriving on a LIVE request (r.Context() is
// still alive — see handler_budget_deadline_test.go's package doc for
// why that is the state under test, not an already-expired r.Context()).
type deadlineMarketSourceReader struct{}

func (deadlineMarketSourceReader) PairSourceStats(context.Context, []string, []string) ([]timescale.SourceStats, error) {
	return nil, deadlineOnLiveRequestErr("scanSourceStats")
}

func (deadlineMarketSourceReader) AssetSourceStats(context.Context, []string) ([]timescale.SourceStats, error) {
	return nil, deadlineOnLiveRequestErr("scanSourceStats")
}

// brokenMarketSourceReader fails with a plain read error carrying no
// deadline signal — the other half of the contract pinned below.
type brokenMarketSourceReader struct{}

func (brokenMarketSourceReader) PairSourceStats(context.Context, []string, []string) ([]timescale.SourceStats, error) {
	return nil, io.ErrUnexpectedEOF
}

func (brokenMarketSourceReader) AssetSourceStats(context.Context, []string) ([]timescale.SourceStats, error) {
	return nil, io.ErrUnexpectedEOF
}
