package pipeline

import (
	"context"
	"fmt"
	"log/slog"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// ProcessLedger runs the dispatcher over one LedgerCloseMeta and forwards each emitted event to sink.
// A dispatcher error is logged and returned unchanged so the caller refuses cursor advancement; ctx cancellation returns ctx.Err() (shutdown).
// The caller persists the cursor only after a nil return.
func ProcessLedger(
	ctx context.Context,
	disp *dispatcher.Dispatcher,
	events chan<- consumer.Event,
	logger *slog.Logger,
	lcm sdkxdr.LedgerCloseMeta,
	networkPassphrase string,
) (err error) {
	before := disp.Stats()
	defer func() {
		emitDispatcherMetricDeltas(before, disp.Stats())
		if r := recover(); r != nil {
			err = fmt.Errorf("dispatcher panic for ledger %d: %v", lcm.LedgerSequence(), r)
			logger.Warn("dispatcher panicked",
				"ledger", lcm.LedgerSequence(),
				"panic", fmt.Sprintf("%v", r),
			)
		}
	}()
	outputs, err := disp.ProcessLedger(lcm, networkPassphrase)
	if err != nil {
		logger.Warn("dispatcher rejected ledger",
			"ledger", lcm.LedgerSequence(),
			"err", err,
		)
		return err
	}
	for _, ev := range outputs {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case events <- ev:
		}
	}
	return nil
}

func emitDispatcherMetricDeltas(before, after dispatcher.Stats) {
	for source, n := range after.EventsSeen {
		delta := n - before.EventsSeen[source]
		if delta <= 0 {
			continue
		}
		obs.SourceMatchedEventsTotal.WithLabelValues(source).Add(float64(delta))
	}
	for source, n := range after.DecodeErrors {
		delta := n - before.DecodeErrors[source]
		if delta <= 0 {
			continue
		}
		obs.SourceDecodeErrorsTotal.WithLabelValues(source).Add(float64(delta))
	}
	for source, n := range after.OrphanEvents {
		delta := n - before.OrphanEvents[source]
		if delta <= 0 {
			continue
		}
		obs.SourceOrphanEventsTotal.WithLabelValues(source).Add(float64(delta))
	}
	// A priced event dropped for want of a pair/pool token mapping decoded cleanly, so it bypasses the DecodeErrors path above;
	// count it against the same budget so the loss is not silent.
	for source, n := range after.UnknownContractDrops {
		delta := n - before.UnknownContractDrops[source]
		if delta <= 0 {
			continue
		}
		obs.SourceDecodeErrorsTotal.WithLabelValues(source).Add(float64(delta))
	}
	// Non-directional swaps are a recognized non-trade class, not lost
	// data (ADR-0033 expected-zero) — their own counter, not folded into
	// DecodeErrors.
	for source, n := range after.NonDirectionalSwaps {
		delta := n - before.NonDirectionalSwaps[source]
		if delta <= 0 {
			continue
		}
		obs.SourceNonDirectionalSwapsTotal.WithLabelValues(source).Add(float64(delta))
	}
}
