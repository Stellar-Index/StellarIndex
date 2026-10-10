package v1_test

import (
	"context"
	"errors"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// errChartReadBroke is a plain read failure carrying no deadline signal,
// so the deadline and non-deadline halves below are told apart by the
// request context alone.
var errChartReadBroke = errors.New("prices_1d: broke")

// stallingMarketCapHistory blocks its price-series read until the caller's
// context is done, then returns that context's error — how a cold
// prices_1d scan behaves when a deadline beats it. Everything else is the
// ordinary stub, so only the market-cap price leg stalls.
type stallingMarketCapHistory struct{ *stubHistoryReader }

func (stallingMarketCapHistory) HistoryPointsInRange(
	ctx context.Context, _ canonical.Pair, _ string, _, _ time.Time, _ int,
) ([]v1.HistoryPoint, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// stallingFXHistory blocks until the request budget expires, the way a
// slow fx_quotes read does, and returns the context's own error.
type stallingFXHistory struct{}

func (stallingFXHistory) ListFXHistory(ctx context.Context, _ string, _, _ time.Time) ([]v1.FXQuotePoint, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
