package v1

import (
	"context"
	"io"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// deadlineMarketSourceReader fails the per-source aggregate with an error wrapping context.DeadlineExceeded on a live
// request (r.Context() still alive), the state under test per handler_budget_deadline_test.go.
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
