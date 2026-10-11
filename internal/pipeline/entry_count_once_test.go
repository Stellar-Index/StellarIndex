package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/defindex"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
)

// TestHandleEvent_EntryCountFollowsTheLandedInsert: the `entries` bump must run only AFTER the row landed, or infra retries and rejected rows over-count.
// The nil store makes the order observable: a correct order surfaces the empty-TxHash rejection, a bump ahead of it dereferences nil (ErrSinkPanic).
func TestHandleEvent_EntryCountFollowsTheLandedInsert(t *testing.T) {
	cases := []struct {
		name string
		ev   consumer.Event
	}{
		{"soroswap_router.Event", soroswap_router.Event{Swap: soroswap_router.RouterSwap{Source: soroswap_router.SourceName}}},
		{"defindex.Event", defindex.Event{Flow: defindex.StrategyFlow{Source: defindex.SourceName}}},
		{"defindex.VaultEvent", defindex.VaultEvent{Flow: defindex.VaultFlow{Source: defindex.SourceName}}},
		{"defindex.DFeesEvent", defindex.DFeesEvent{Fee: defindex.DFee{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := handleEvent(context.Background(), discardLogger(), nil, tc.ev, true)
			if err == nil {
				t.Fatal("handleEvent returned nil for a row the store must reject")
			}
			if errors.Is(err, ErrSinkPanic) {
				t.Fatalf("the entries bump ran before the store's verdict (nil store dereferenced, recovered as a sink panic): %v", err)
			}
			if !strings.Contains(err.Error(), "TxHash is empty") {
				t.Fatalf("err = %v, want the store's own pre-SQL rejection", err)
			}
		})
	}
}

// TestHandleEvent_PermanentlyInvalidTradeReturnsDrop drives the production entry point the projector's sink binds (pipeline.HandleEvent);
// a zero-value trade fails Validate before any SQL, so the nil store is never dereferenced.
func TestHandleEvent_PermanentlyInvalidTradeReturnsDrop(t *testing.T) {
	ev := soroswap.TradeEvent{Trade: canonical.Trade{Source: "soroswap", Ledger: 703}}
	err := HandleEvent(context.Background(), discardLogger(), nil, ev)
	if err == nil {
		t.Fatal("HandleEvent returned nil for a trade the store permanently rejected — the projector counts it emitted/ok")
	}
	var dropped *TradeDroppedError
	if !errors.As(err, &dropped) {
		t.Fatalf("err = %T (%v); want *TradeDroppedError", err, err)
	}
	if !errors.Is(err, canonical.ErrInvalidTrade) {
		t.Errorf("err = %v; want it to wrap canonical.ErrInvalidTrade (the projector's value-shape skip arm)", err)
	}
}
