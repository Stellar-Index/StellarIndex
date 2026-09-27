package redisclient

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// errorMetricsHook counts failed commands into
// [obs.RedisCommandErrorsTotal]. [Build] installs it on every client;
// many callers treat Redis writes as best-effort and drop the error, so
// this is the only place a write-refusing Redis (MISCONF, OOM) shows up.
type errorMetricsHook struct{}

var knownRedisClass = func() map[string]struct{} {
	m := make(map[string]struct{}, len(obs.RedisErrorClasses))
	for _, c := range obs.RedisErrorClasses {
		m[c] = struct{}{}
	}
	return m
}()

// DialHook passes through: a dial failure surfaces as the command's error.
func (errorMetricsHook) DialHook(next redis.DialHook) redis.DialHook { return next }

// ProcessHook counts the command's final error, after go-redis retries.
func (errorMetricsHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		recordCommandError(err)
		return err
	}
}

// ProcessPipelineHook counts each failed command in the pipeline, except
// that a transport failure (which every queued command inherits) counts once
// and a transaction's MULTI/EXEC wrappers are not counted.
func (errorMetricsHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		err := next(ctx, cmds)
		var rErr redis.Error
		if err != nil && !errors.As(err, &rErr) {
			recordCommandError(err)
			return err
		}
		for _, cmd := range cmds {
			if name := cmd.Name(); name == "multi" || name == "exec" {
				continue
			}
			recordCommandError(cmd.Err())
		}
		return err
	}
}

func recordCommandError(err error) {
	if class, ok := classifyError(err); ok {
		obs.RedisCommandErrorsTotal.WithLabelValues(class).Inc()
	}
}

// classifyError maps err to a bounded class label. ok is false for
// outcomes that are not failures: nil, redis.Nil (key absent), and
// NOSCRIPT, which Script.Run answers by falling back to EVAL.
func classifyError(err error) (class string, ok bool) {
	if err == nil || errors.Is(err, redis.Nil) {
		return "", false
	}
	var rErr redis.Error
	if errors.As(err, &rErr) {
		msg := rErr.Error()
		prefix, _, _ := strings.Cut(msg, " ")
		if prefix == "NOSCRIPT" {
			return "", false
		}
		if _, known := knownRedisClass[prefix]; known && prefix == strings.ToUpper(prefix) {
			return prefix, true
		}
		return "other", true
	}
	if errors.Is(err, context.Canceled) {
		return "canceled", true
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, redis.ErrPoolTimeout) ||
		(errors.As(err, &netErr) && netErr.Timeout()) {
		return "timeout", true
	}
	return "io", true
}
