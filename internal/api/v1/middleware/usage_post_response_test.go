package middleware_test

import (
	"context"
	"net"
	"time"

	"github.com/redis/go-redis/v9"
)

// slowRedisHook delays every command by delay, simulating a wedged Redis
// without needing a real network stall. Cooperates with ctx
// cancellation so it never masks the fact that the request's own
// deadline still applies to anything that DOES stay on the request's
// context.
type slowRedisHook struct{ delay time.Duration }

func (slowRedisHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h slowRedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		select {
		case <-time.After(h.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
		return next(ctx, cmd)
	}
}

func (slowRedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

type recordingToucher struct{ calls int }

func (r *recordingToucher) TouchUsage(ctx context.Context, _ string, _ net.IP, _ string) error {
	if err := ctx.Err(); err != nil {
		return err // a cancelled ctx must not reach here
	}
	r.calls++
	return nil
}

type alwaysTouch struct{}

func (alwaysTouch) ShouldTouch(ctx context.Context, _ string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return true, nil
}
