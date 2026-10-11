package sdex

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// TradeEvent.EventKind / Source and Decoder.Name are trivial accessors but load-bearing: the
// consumer-layer event sink type-switches on EventKind to pick the metric label, and the
// dispatcher maps Decoder.Name to the per-source decode-error counter. Changing the value would
// silently misroute telemetry.
func TestTradeEvent_implementsConsumerEvent(t *testing.T) {
	te := TradeEvent{}
	if got := te.EventKind(); got != "sdex.trade" {
		t.Errorf("EventKind() = %q, want \"sdex.trade\"", got)
	}
	if got := te.Source(); got != SourceName {
		t.Errorf("Source() = %q, want %q", got, SourceName)
	}
	var _ consumer.Event = te
}
