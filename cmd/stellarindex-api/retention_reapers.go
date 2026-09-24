package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/retentionreaper"
)

// sessionSweeper and deliverySweeper are the narrow seams the retention
// reapers bind to, satisfied by the Postgres user and webhook stores.
type sessionSweeper interface {
	SweepEndedSessions(ctx context.Context, olderThan time.Time) (int64, error)
}

type deliverySweeper interface {
	SweepFinishedDeliveries(ctx context.Context, olderThan time.Time) (int64, error)
}

// retentionReaperTargets returns one reaper per platform table the
// dashboard bundle writes and nothing else bounds. A store without the
// sweep seam (dashboard not wired, or a non-Postgres fake) yields none.
func retentionReaperTargets(b dashboardBundle, logger *slog.Logger) []retentionreaper.Options {
	var out []retentionreaper.Options
	if s, ok := b.users.(sessionSweeper); ok && s != nil {
		out = append(out, retentionreaper.Options{
			Name:      obs.AuthReaperSession,
			Sweep:     s.SweepEndedSessions,
			Retention: retentionreaper.SessionRetention,
			Logger:    logger.With("component", "session-reaper"),
		})
	}
	if d, ok := b.webhookStore.(deliverySweeper); ok && d != nil {
		out = append(out, retentionreaper.Options{
			Name:      obs.AuthReaperWebhookDelivery,
			Sweep:     d.SweepFinishedDeliveries,
			Retention: retentionreaper.WebhookDeliveryRetention,
			Logger:    logger.With("component", "webhook-delivery-reaper"),
		})
	}
	return out
}

// startRetentionReapers runs each target on the background wait group,
// bounded to ctx.
func startRetentionReapers(ctx context.Context, wg *sync.WaitGroup, logger *slog.Logger, targets []retentionreaper.Options) {
	for _, opts := range targets {
		r := retentionreaper.New(opts)
		worker := opts.Name + "-retention-reaper"
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer recoverBackgroundWorker(logger, worker)
			if err := r.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error(worker+" worker exited", "err", err)
			}
		}()
	}
}
