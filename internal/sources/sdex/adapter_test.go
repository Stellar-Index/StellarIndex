package sdex

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// TestTradeEvent_implementsConsumerEvent pins EventKind, Source and Decoder.Name: the event sink type-switches on EventKind
// for the metric label, and the dispatcher keys the per-source decode-error counter on Decoder.Name, so a changed value
// silently misroutes telemetry.
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
