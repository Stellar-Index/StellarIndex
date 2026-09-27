package bitstamp

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestRecordSubscriptionAck_RejectionSetsGaugeAndLogs is the
// CA2-A18-harden-3 guard: a rejected bts:error subscription must flip
// obs.CEXStreamSubscriptionRejected and log, not vanish silently the
// way "swallow and continue" used to leave it.
func TestRecordSubscriptionAck_RejectionSetsGaugeAndLogs(t *testing.T) {
	// Distinct pair from other bitstamp tests so the shared gauge's
	// state can't collide across parallel test runs.
	const rejectedSymbol = "xlmbtc_ca2a18h3"

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	recordSubscriptionAck(logger, &subscriptionAck{
		Channel:  ChannelPrefix + rejectedSymbol,
		Message:  "Bad subscription string.",
		Accepted: false,
	})

	if v := testutil.ToFloat64(obs.CEXStreamSubscriptionRejected.WithLabelValues(SourceName, rejectedSymbol)); v != 1 {
		t.Errorf("subscription_rejected{symbol=%s} = %v, want 1", rejectedSymbol, v)
	}
	if got := logs.String(); !strings.Contains(got, "rejected") {
		t.Errorf("expected a rejection log line, got %q", got)
	}

	recordSubscriptionAck(logger, &subscriptionAck{
		Channel:  ChannelPrefix + rejectedSymbol,
		Accepted: true,
	})
	if v := testutil.ToFloat64(obs.CEXStreamSubscriptionRejected.WithLabelValues(SourceName, rejectedSymbol)); v != 0 {
		t.Errorf("subscription_rejected{symbol=%s} = %v, want 0 after acceptance clears it", rejectedSymbol, v)
	}
}
