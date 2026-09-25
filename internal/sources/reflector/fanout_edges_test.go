package reflector

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// The fanout guards are inclusive-exclusive: exactly opIndexFanoutStride
// slots and EventIndex eventFanoutStride-1 fit; one more of either would
// spill into the next block's OpIndex range.
func TestDecodeUpdate_FanoutEdges(t *testing.T) {
	prev, prevTS := decodeUpdateBody, decodeUpdateTimestamp
	defer func() { decodeUpdateBody, decodeUpdateTimestamp = prev, prevTS }()
	decodeUpdateTimestamp = func(_ string) (uint64, error) { return 0, nil }

	full := make([]PriceEntry, opIndexFanoutStride)
	for i := range full {
		full[i] = PriceEntry{Asset: canonical.NativeAsset(), Price: canonical.NewAmount(big.NewInt(1))}
	}
	decodeUpdateBody = func(_ string) ([]PriceEntry, error) { return full, nil }

	e := &events.Event{
		Topic:          []string{TopicSymbolReflector, TopicSymbolUpdate, "ts"},
		ContractID:     dexContractID,
		OperationIndex: 2,
		EventIndex:     eventFanoutStride - 1,
	}
	updates, err := decodeUpdate(e, VariantDEX, DefaultDecimals, "", time.Now())
	if err != nil {
		t.Fatalf("a full %d-slot vector at the last EventIndex must decode: %v", opIndexFanoutStride, err)
	}
	if len(updates) != opIndexFanoutStride {
		t.Fatalf("got %d updates, want %d", len(updates), opIndexFanoutStride)
	}
	base := uint32((2*eventFanoutStride + eventFanoutStride - 1) * opIndexFanoutStride)
	if first, last := updates[0].OpIndex, updates[len(updates)-1].OpIndex; first != base || last != base+opIndexFanoutStride-1 {
		t.Fatalf("OpIndex range = [%d, %d], want [%d, %d]", first, last, base, base+opIndexFanoutStride-1)
	}

	e.EventIndex = eventFanoutStride
	if _, err := decodeUpdate(e, VariantDEX, DefaultDecimals, "", time.Now()); !errors.Is(err, ErrEventIndexOverflow) {
		t.Fatalf("EventIndex %d: got %v, want ErrEventIndexOverflow", eventFanoutStride, err)
	}
}

// OperationIndex is the third packing input: at 1<<16 the packed value is
// exactly 2^32 and wraps onto operation 0's OpIndex block, which the
// oracle_updates upsert would then overwrite.
func TestDecodeUpdate_OperationIndexBound(t *testing.T) {
	prev, prevTS := decodeUpdateBody, decodeUpdateTimestamp
	defer func() { decodeUpdateBody, decodeUpdateTimestamp = prev, prevTS }()
	decodeUpdateTimestamp = func(_ string) (uint64, error) { return 0, nil }
	decodeUpdateBody = func(_ string) ([]PriceEntry, error) {
		return []PriceEntry{{Asset: canonical.NativeAsset(), Price: canonical.NewAmount(big.NewInt(1))}}, nil
	}
	e := &events.Event{
		Topic:          []string{TopicSymbolReflector, TopicSymbolUpdate, "ts"},
		ContractID:     dexContractID,
		OperationIndex: 1<<16 - 1,
		EventIndex:     eventFanoutStride - 1,
	}
	updates, err := decodeUpdate(e, VariantDEX, DefaultDecimals, "", time.Now())
	if err != nil {
		t.Fatalf("the last in-range OperationIndex must decode: %v", err)
	}
	if want := uint32((1<<16-1)*eventFanoutStride+eventFanoutStride-1) * opIndexFanoutStride; updates[0].OpIndex != want {
		t.Fatalf("OpIndex = %d, want %d", updates[0].OpIndex, want)
	}
	for _, op := range []int{1 << 16, -1} {
		e.OperationIndex, e.EventIndex = op, 0
		if got, err := decodeUpdate(e, VariantDEX, DefaultDecimals, "", time.Now()); !errors.Is(err, ErrOperationIndexOverflow) {
			t.Errorf("OperationIndex %d: got (%v, %v), want ErrOperationIndexOverflow", op, got, err)
		}
	}
}
