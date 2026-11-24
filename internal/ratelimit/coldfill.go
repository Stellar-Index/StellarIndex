package ratelimit

import (
	"context"
	"errors"
)

// ColdFillCost is the extra tokens a request pays for each cold cache fill it starts: a never-computed client-chosen key whose
// detached lake read it owns. A refresh-gate class serves ~2 such reads at a time (120-600/min), so at 5 an anonymous IP's
// 1000/min buys ~62 cold contract pages (three fills each) and cannot hold a class alone; a warm read still costs 1.
const ColdFillCost = 5

// ErrColdFillRefused is what a cold read returns when its request could not pay [ColdFillCost]: no fill was started and no
// refresh-gate slot claimed. The handler answers the limiter's 429 (middleware.WriteRateLimitRefusal), never a 5xx.
var ErrColdFillRefused = errors.New("ratelimit: cold cache fill refused; request budget exhausted")

type coldFillChargeKey struct{}

// WithColdFillCharge attaches the request's cold-fill charge: charge bills tokens more, or returns an error wrapping
// [ErrColdFillRefused] when the request may not proceed. A nil charge exempts ctx's reads.
func WithColdFillCharge(ctx context.Context, charge func(tokens int) error) context.Context {
	return context.WithValue(ctx, coldFillChargeKey{}, charge)
}

// ChargeColdFill bills ctx's request [ColdFillCost] for a cold fill it is about to start, before it claims any slot. With no charge
// in ctx (limiter disabled, prewarmers, internal callers) it is free.
func ChargeColdFill(ctx context.Context) error {
	charge, ok := ctx.Value(coldFillChargeKey{}).(func(int) error)
	if !ok || charge == nil {
		return nil
	}
	return charge(ColdFillCost)
}
