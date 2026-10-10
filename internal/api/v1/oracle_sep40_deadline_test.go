package v1

import (
	"context"
	"io"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// deadlinePriceReader fails every PriceReader call the way an expired
// per-call budget does: the driver's error WRAPPING
// context.DeadlineExceeded, arriving on a LIVE request (r.Context() is
// still alive — see handler_budget_deadline_test.go's package doc for why
// that is the state under test).
type deadlinePriceReader struct{}

func (deadlinePriceReader) LatestPrice(context.Context, canonical.Asset, canonical.Asset) (PriceSnapshot, []string, bool, error) {
	return PriceSnapshot{}, nil, false, deadlineOnLiveRequestErr("read price")
}

func (deadlinePriceReader) RecentClosedSnapshots(context.Context, canonical.Asset, canonical.Asset, int) ([]PriceSnapshot, error) {
	return nil, deadlineOnLiveRequestErr("read closed snapshots")
}

// brokenPriceReader fails with a plain read error carrying no deadline
// signal — the other half of the contract pinned above.
type brokenPriceReader struct{}

func (brokenPriceReader) LatestPrice(context.Context, canonical.Asset, canonical.Asset) (PriceSnapshot, []string, bool, error) {
	return PriceSnapshot{}, nil, false, io.ErrUnexpectedEOF
}

func (brokenPriceReader) RecentClosedSnapshots(context.Context, canonical.Asset, canonical.Asset, int) ([]PriceSnapshot, error) {
	return nil, io.ErrUnexpectedEOF
}
