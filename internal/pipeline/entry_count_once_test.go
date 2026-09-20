package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/defindex"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
)

// TestHandleEvent_EntryCountFollowsTheLandedInsert pins the ordering the
// four inline handleEvent cases share with every persist helper: the
// `entries` bump runs only AFTER the row landed. Bumping ahead of the
// insert counted one entry per REL-08 infra-retry attempt of the same
// event, and one for a row the store then rejected (Q062).
//
// The seam is the nil store the validation-reject tests already use: each
// store.Insert* rejects an empty TxHash in Go before touching the pool, so
// the corrected order surfaces that verdict, whereas a bump ahead of it
// dereferences the nil store — recovered by handleEvent as ErrSinkPanic.
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
