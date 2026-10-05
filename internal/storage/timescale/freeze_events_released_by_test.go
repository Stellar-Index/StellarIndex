package timescale

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// A nil db proves the refusal happens before any SQL runs.
func TestMarkRecovered_RejectsMalformedReleasedBy(t *testing.T) {
	sink := &FreezeEventSink{}
	asset := canonical.NativeAsset()
	quote, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	for _, v := range []string{"", "ash", "operator:", "system: ", "Operator:ash", "admin:ash"} {
		if err := sink.MarkRecovered(context.Background(), asset, quote, v); err == nil {
			t.Errorf("MarkRecovered(released_by=%q) = nil, want a refusal", v)
		}
	}
}

func TestValidReleasedBy_AcceptsBothKinds(t *testing.T) {
	for _, v := range []string{"operator:ash", "system:recovery"} {
		if !validReleasedBy(v) {
			t.Errorf("validReleasedBy(%q) = false, want true", v)
		}
	}
}
