package pipeline

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestEmitDispatcherMetricDeltas_UnknownContractDropsWiring: a decoder-reported UnknownContractDrops delta must reach obs.SourceDecodeErrorsTotal, or the drop reaches no metric.
func TestEmitDispatcherMetricDeltas_UnknownContractDropsWiring(t *testing.T) {
	const source = "soroswap"
	before := testutil.ToFloat64(obs.SourceDecodeErrorsTotal.WithLabelValues(source))

	emitDispatcherMetricDeltas(
		dispatcher.Stats{UnknownContractDrops: map[string]int{source: 0}},
		dispatcher.Stats{UnknownContractDrops: map[string]int{source: 3}},
	)

	got := testutil.ToFloat64(obs.SourceDecodeErrorsTotal.WithLabelValues(source)) - before
	if got != 3 {
		t.Errorf("SourceDecodeErrorsTotal delta = %v, want 3", got)
	}
}

// TestEmitDispatcherMetricDeltas_ErrorSeriesExistBeforeFirstError: a source that matched events must export its error series at 0,
// or the first failure burst is the series' first sample and rate()-based alerts never see it.
func TestEmitDispatcherMetricDeltas_ErrorSeriesExistBeforeFirstError(t *testing.T) {
	const source = "presence-probe"
	decodeBefore := testutil.CollectAndCount(obs.SourceDecodeErrorsTotal)
	orphanBefore := testutil.CollectAndCount(obs.SourceOrphanEventsTotal)

	emitDispatcherMetricDeltas(dispatcher.Stats{}, dispatcher.Stats{EventsSeen: map[string]int{source: 5}})

	if got := testutil.CollectAndCount(obs.SourceDecodeErrorsTotal) - decodeBefore; got != 1 {
		t.Errorf("SourceDecodeErrorsTotal gained %d series, want 1", got)
	}
	if got := testutil.CollectAndCount(obs.SourceOrphanEventsTotal) - orphanBefore; got != 1 {
		t.Errorf("SourceOrphanEventsTotal gained %d series, want 1", got)
	}
	if v := testutil.ToFloat64(obs.SourceDecodeErrorsTotal.WithLabelValues(source)); v != 0 {
		t.Errorf("SourceDecodeErrorsTotal{%s} = %v, want 0", source, v)
	}
}

func TestProcessLedger_ReturnsDispatcherError(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	disp := dispatcher.New()
	events := make(chan consumer.Event)
	defer close(events)

	err := ProcessLedger(
		context.Background(),
		disp,
		events,
		logger,
		invalidLedgerCloseMeta(42),
		"not-a-real-network-passphrase",
	)
	if err == nil {
		t.Fatal("expected dispatcher/build-reader error for invalid ledger meta")
	}
}

func invalidLedgerCloseMeta(seq uint32) sdkxdr.LedgerCloseMeta {
	component := sdkxdr.TxSetComponent{
		Type: sdkxdr.TxSetComponentTypeTxsetCompTxsMaybeDiscountedFee,
		TxsMaybeDiscountedFee: &sdkxdr.TxSetComponentTxsMaybeDiscountedFee{
			Txs: []sdkxdr.TransactionEnvelope{{}},
		},
	}
	components := []sdkxdr.TxSetComponent{component}
	return sdkxdr.LedgerCloseMeta{
		V: 1,
		V1: &sdkxdr.LedgerCloseMetaV1{
			LedgerHeader: sdkxdr.LedgerHeaderHistoryEntry{
				Header: sdkxdr.LedgerHeader{
					LedgerSeq: sdkxdr.Uint32(seq),
				},
			},
			TxSet: sdkxdr.GeneralizedTransactionSet{
				V: 1,
				V1TxSet: &sdkxdr.TransactionSetV1{
					Phases: []sdkxdr.TransactionPhase{{
						V:            0,
						V0Components: &components,
					}},
				},
			},
		},
	}
}
