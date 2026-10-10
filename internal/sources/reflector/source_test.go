package reflector

import (
	"encoding/base64"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

const (
	reflectorTxHash = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	dexContractID   = "CAFF00000000000000000000000000000000000000000000000000000"
	cexContractID   = "CBAR00000000000000000000000000000000000000000000000000000"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		topics []string
		want   bool
	}{
		{"match", []string{TopicSymbolReflector, TopicSymbolUpdate}, true},
		{"wrong topic0", []string{"elsewhere", TopicSymbolUpdate}, false},
		{"wrong topic1", []string{TopicSymbolReflector, "create"}, false},
		{"missing topic1", []string{TopicSymbolReflector}, false},
		{"empty topics", []string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(&events.Event{Topic: tc.topics})
			if got != tc.want {
				t.Errorf("classify = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestVariantSourceName(t *testing.T) {
	cases := map[Variant]string{
		VariantDEX:  SourceDEX,
		VariantCEX:  SourceCEX,
		VariantFX:   SourceFX,
		Variant(99): "reflector-unknown",
	}
	for v, want := range cases {
		if got := v.SourceName(); got != want {
			t.Errorf("%d.SourceName() = %q, want %q", v, got, want)
		}
	}
}

func TestQuoteForVariant(t *testing.T) {
	// CEX and FX publish an explicit USD base. DEX (CALI2BYU…) is
	// denominated in whatever its SEP-40 base() returns — the pubnet USDC
	// SAC — so it must be stamped with that SAC, never fiat:USD (ingest-time
	// stablecoin normalisation hides a depeg) and never native (XLM).
	usd, _ := canonical.NewFiatAsset("USD")
	usdcSAC, err := canonical.NewSorobanAsset("CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75")
	if err != nil {
		t.Fatalf("NewSorobanAsset: %v", err)
	}
	native := canonical.NativeAsset()
	want := map[Variant]canonical.Asset{
		VariantDEX: usdcSAC,
		VariantCEX: usd,
		VariantFX:  usd,
	}
	for v, w := range want {
		q := quoteForVariant(v)
		if !q.Equal(w) {
			t.Errorf("%s quote = %s, want %s", v.SourceName(), q, w)
		}
		if q.Equal(native) {
			t.Errorf("%s quote must NOT be native (XLM)", v.SourceName())
		}
	}
}

func TestDecodeUpdate_fanout(t *testing.T) {
	// Fake decoder returns three (asset, price) pairs — expect 3
	// canonical.OracleUpdate records back.
	prev, prevTS := decodeUpdateBody, decodeUpdateTimestamp
	defer func() { decodeUpdateBody, decodeUpdateTimestamp = prev, prevTS }()

	xlm := canonical.NativeAsset()
	usdcAsset, _ := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")

	decodeUpdateBody = func(_ string) ([]PriceEntry, error) {
		v := func(s string) canonical.Amount {
			n, _ := new(big.Int).SetString(s, 10)
			return canonical.NewAmount(n)
		}
		return []PriceEntry{
			{Asset: xlm, Price: v("1242000000000000")},      // 0.1242 USD (at 14 decimals) — in DEX it's XLM/XLM which doesn't make sense but fine for the test
			{Asset: usdcAsset, Price: v("100000000000000")}, // 1 USDC
			{Asset: xlm, Price: v("1243000000000000")},      // duplicate asset — different OpIndex
		}, nil
	}
	// Reflector event timestamps are u64 milliseconds.
	// 1745000000000 ms = 2025-04-18T17:33:20Z.
	decodeUpdateTimestamp = func(_ string) (uint64, error) { return 1745000000000, nil }

	e := &events.Event{
		Topic:          []string{TopicSymbolReflector, TopicSymbolUpdate, "ts-placeholder"},
		ContractID:     cexContractID,
		Ledger:         52_430_001,
		TxHash:         reflectorTxHash,
		OperationIndex: 0,
		LedgerClosedAt: time.Now().UTC().Format(time.RFC3339),
	}
	closedAt, _ := time.Parse(time.RFC3339, e.LedgerClosedAt)

	updates, err := decodeUpdate(e, VariantCEX, DefaultDecimals, "GRELAYER", closedAt)
	if err != nil {
		t.Fatalf("decodeUpdate: %v", err)
	}
	if len(updates) != 3 {
		t.Fatalf("expected 3 updates, got %d", len(updates))
	}

	// Source name stamped per variant.
	for _, u := range updates {
		if u.Source != SourceCEX {
			t.Errorf("Source = %q, want %q", u.Source, SourceCEX)
		}
		if u.Observer != "GRELAYER" {
			t.Errorf("Observer = %q", u.Observer)
		}
		if u.Decimals != DefaultDecimals {
			t.Errorf("Decimals = %d", u.Decimals)
		}
		if u.ContractID != cexContractID {
			t.Errorf("ContractID = %q", u.ContractID)
		}
	}

	// Timestamp sourced from the oracle's own timestamp (not ledger close).
	// Event carries 1745000000000 ms → 1745000000 seconds unix.
	if updates[0].Timestamp.UnixMilli() != 1745000000000 {
		t.Errorf("timestamp wrong: %v (%d ms)", updates[0].Timestamp, updates[0].Timestamp.UnixMilli())
	}

	// Each update has a distinct OpIndex to keep identity unique.
	seenOps := map[uint32]bool{}
	for _, u := range updates {
		if seenOps[u.OpIndex] {
			t.Errorf("duplicate OpIndex %d", u.OpIndex)
		}
		seenOps[u.OpIndex] = true
	}

	// Quote is fiat:USD for CEX.
	usd, _ := canonical.NewFiatAsset("USD")
	for _, u := range updates {
		if !u.Quote.Equal(usd) {
			t.Errorf("Quote = %+v, want fiat:USD", u.Quote)
		}
	}
}

func TestDecodeUpdate_OpIndexStrideIsFixed(t *testing.T) {
	// Regression guard: an OpIndex of `OperationIndex × len(prices) + i`
	// could collide across events in the same tx with different vector
	// sizes. With a fixed stride (opIndexFanoutStride),
	// the op_index ranges never overlap.
	prev, prevTS := decodeUpdateBody, decodeUpdateTimestamp
	defer func() { decodeUpdateBody, decodeUpdateTimestamp = prev, prevTS }()
	decodeUpdateTimestamp = func(_ string) (uint64, error) { return 0, nil }

	xlm := canonical.NativeAsset()
	usdc, _ := canonical.NewFiatAsset("USD")
	v := func(s string) canonical.Amount {
		n, _ := new(big.Int).SetString(s, 10)
		return canonical.NewAmount(n)
	}

	// Event A: op_index=0 with 3 prices.
	decodeUpdateBody = func(_ string) ([]PriceEntry, error) {
		return []PriceEntry{
			{Asset: xlm, Price: v("100")},
			{Asset: usdc, Price: v("100")},
			{Asset: xlm, Price: v("100")},
		}, nil
	}
	eA := &events.Event{
		Topic:      []string{TopicSymbolReflector, TopicSymbolUpdate, "ts"},
		ContractID: dexContractID,
		Ledger:     1, TxHash: reflectorTxHash, OperationIndex: 0,
		LedgerClosedAt: time.Now().UTC().Format(time.RFC3339),
	}
	closedAt, _ := time.Parse(time.RFC3339, eA.LedgerClosedAt)
	updatesA, err := decodeUpdate(eA, VariantDEX, DefaultDecimals, "", closedAt)
	if err != nil {
		t.Fatal(err)
	}

	// Event B: op_index=1 with 2 prices — if the old formula was
	// still in place, B's first slot would be OpIndex=2 which
	// collides with A's third slot at OpIndex=2 (0×3+2).
	decodeUpdateBody = func(_ string) ([]PriceEntry, error) {
		return []PriceEntry{
			{Asset: xlm, Price: v("100")},
			{Asset: usdc, Price: v("100")},
		}, nil
	}
	eB := &events.Event{
		Topic:      []string{TopicSymbolReflector, TopicSymbolUpdate, "ts"},
		ContractID: dexContractID,
		Ledger:     1, TxHash: reflectorTxHash, OperationIndex: 1,
		LedgerClosedAt: eA.LedgerClosedAt,
	}
	updatesB, err := decodeUpdate(eB, VariantDEX, DefaultDecimals, "", closedAt)
	if err != nil {
		t.Fatal(err)
	}

	seen := map[uint32]bool{}
	for _, u := range updatesA {
		if seen[u.OpIndex] {
			t.Errorf("duplicate OpIndex in A: %d", u.OpIndex)
		}
		seen[u.OpIndex] = true
	}
	for _, u := range updatesB {
		if seen[u.OpIndex] {
			t.Errorf("cross-event OpIndex collision: %d in both A and B", u.OpIndex)
		}
		seen[u.OpIndex] = true
	}
	if len(seen) != len(updatesA)+len(updatesB) {
		t.Errorf("op_index uniqueness violated: %d unique across %d total",
			len(seen), len(updatesA)+len(updatesB))
	}
}

// TestDecodeUpdate_EventIndexPreventsSameOpCollision is the
// regression test for a same-op collision: two
// Reflector update events emitted by the SAME operation
// (OperationIndex equal) but at DIFFERENT positions in that
// operation's contract-event list (EventIndex differs) collide
// if the synthetic OpIndex's fanout base were
// OperationIndex ALONE. The base must incorporate EventIndex too, so
// two same-source events within one op get disjoint 1024-wide
// OpIndex blocks.
func TestDecodeUpdate_EventIndexPreventsSameOpCollision(t *testing.T) {
	prev, prevTS := decodeUpdateBody, decodeUpdateTimestamp
	defer func() { decodeUpdateBody, decodeUpdateTimestamp = prev, prevTS }()
	decodeUpdateTimestamp = func(_ string) (uint64, error) { return 0, nil }

	xlm := canonical.NativeAsset()
	v := func(s string) canonical.Amount {
		n, _ := new(big.Int).SetString(s, 10)
		return canonical.NewAmount(n)
	}
	decodeUpdateBody = func(_ string) ([]PriceEntry, error) {
		return []PriceEntry{{Asset: xlm, Price: v("100")}}, nil
	}

	base := events.Event{
		Topic:      []string{TopicSymbolReflector, TopicSymbolUpdate, "ts"},
		ContractID: dexContractID,
		Ledger:     1, TxHash: reflectorTxHash,
		LedgerClosedAt: time.Now().UTC().Format(time.RFC3339),
	}
	closedAt, _ := time.Parse(time.RFC3339, base.LedgerClosedAt)

	eFirst := base
	eFirst.OperationIndex = 7
	eFirst.EventIndex = 0
	updatesFirst, err := decodeUpdate(&eFirst, VariantDEX, DefaultDecimals, "", closedAt)
	if err != nil {
		t.Fatal(err)
	}

	eSecond := base
	eSecond.OperationIndex = 7 // SAME operation as eFirst
	eSecond.EventIndex = 1     // a DIFFERENT event within it
	updatesSecond, err := decodeUpdate(&eSecond, VariantDEX, DefaultDecimals, "", closedAt)
	if err != nil {
		t.Fatal(err)
	}

	if len(updatesFirst) != 1 || len(updatesSecond) != 1 {
		t.Fatalf("expected 1 update each, got %d and %d", len(updatesFirst), len(updatesSecond))
	}
	if updatesFirst[0].OpIndex == updatesSecond[0].OpIndex {
		t.Errorf("two events in the SAME operation (OperationIndex=7) with different EventIndex (0 vs 1) collided on OpIndex=%d — the fanout base must incorporate EventIndex, not just OperationIndex",
			updatesFirst[0].OpIndex)
	}
}

// TestDecodeUpdate_OpIndexStableAcrossAllowlistState is the regression
// test for op-index stability carried into the oracle
// capture-totality change: decodeUpdate's OpIndex is derived from
// the raw update_data vector POSITION, so the rows a mixed
// known/unknown batch produces for its KNOWN slots must carry the
// same OpIndex regardless of allow-list state — the unknown slot
// is a raw:<symbol> row that consumes position 1 and IS emitted
// (a skipped placeholder would consume the same position and emit
// nothing). Either way the known row at
// position 2 keeps OpIndex 2, never the compacted 1, so a later
// allow-list extension re-derives the same PK and promotes the row
// in place instead of orphaning/duplicating it.
func TestDecodeUpdate_OpIndexStableAcrossAllowlistState(t *testing.T) {
	prev, prevTS := decodeUpdateBody, decodeUpdateTimestamp
	defer func() { decodeUpdateBody, decodeUpdateTimestamp = prev, prevTS }()
	decodeUpdateTimestamp = func(_ string) (uint64, error) { return 0, nil }

	xlm := canonical.NativeAsset()
	usdc, _ := canonical.NewFiatAsset("USD")
	raw, _ := canonical.NewOracleRawAsset("NOTACOIN")
	v := func(s string) canonical.Amount {
		n, _ := new(big.Int).SetString(s, 10)
		return canonical.NewAmount(n)
	}
	// Raw update_data vector has 3 slots: [known, UNKNOWN(raw), known].
	decodeUpdateBody = func(_ string) ([]PriceEntry, error) {
		return []PriceEntry{
			{Asset: xlm, Price: v("100")},
			{Asset: raw, Price: v("7")},
			{Asset: usdc, Price: v("100")},
		}, nil
	}

	e := &events.Event{
		Topic:      []string{TopicSymbolReflector, TopicSymbolUpdate, "ts"},
		ContractID: dexContractID,
		Ledger:     1, TxHash: reflectorTxHash, OperationIndex: 0, EventIndex: 0,
		LedgerClosedAt: time.Now().UTC().Format(time.RFC3339),
	}
	closedAt, _ := time.Parse(time.RFC3339, e.LedgerClosedAt)
	updates, err := decodeUpdate(e, VariantDEX, DefaultDecimals, "", closedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 3 {
		t.Fatalf("expected 3 updates (known, raw, known), got %d", len(updates))
	}
	// The known rows' identity is exactly what the pre-totality Skip
	// decoder produced: slot 0 → OpIndex 0, slot 2 → OpIndex 2.
	if got, want := updates[0].OpIndex, uint32(0); got != want || !updates[0].Asset.Equal(xlm) {
		t.Errorf("updates[0] = (%s, OpIndex %d), want (%s, %d)", updates[0].Asset, got, xlm, want)
	}
	if got, want := updates[2].OpIndex, uint32(2); got != want || !updates[2].Asset.Equal(usdc) {
		t.Errorf("updates[2] = (%s, OpIndex %d), want (%s, %d) — raw vector position 2, not compacted position 1",
			updates[2].Asset, got, usdc, want)
	}
	// The raw row fills the unknown symbol's slot.
	if got, want := updates[1].OpIndex, uint32(1); got != want || !updates[1].Asset.Equal(raw) {
		t.Errorf("updates[1] = (%s, OpIndex %d), want (%s, %d)", updates[1].Asset, got, raw, want)
	}
}

func TestDecodeUpdate_skipsZeroPrices(t *testing.T) {
	prev, prevTS := decodeUpdateBody, decodeUpdateTimestamp
	defer func() { decodeUpdateBody, decodeUpdateTimestamp = prev, prevTS }()
	decodeUpdateTimestamp = func(_ string) (uint64, error) { return 0, nil }

	xlm := canonical.NativeAsset()
	decodeUpdateBody = func(_ string) ([]PriceEntry, error) {
		return []PriceEntry{
			{Asset: xlm, Price: canonical.NewAmount(big.NewInt(0))},
			{Asset: xlm, Price: canonical.NewAmount(big.NewInt(100))},
		}, nil
	}

	e := &events.Event{
		Topic:      []string{TopicSymbolReflector, TopicSymbolUpdate, "ts"},
		ContractID: dexContractID,
		Ledger:     1, TxHash: reflectorTxHash, OperationIndex: 0,
		LedgerClosedAt: time.Now().UTC().Format(time.RFC3339),
	}
	closedAt, _ := time.Parse(time.RFC3339, e.LedgerClosedAt)
	updates, err := decodeUpdate(e, VariantDEX, DefaultDecimals, "", closedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 {
		t.Fatalf("expected 1 (zero-price skipped), got %d", len(updates))
	}
}

func TestDecodeUpdate_refusesWrongTopic(t *testing.T) {
	e := &events.Event{Topic: []string{"wrong", "update"}}
	_, err := decodeUpdate(e, VariantDEX, DefaultDecimals, "", time.Now())
	if !errors.Is(err, ErrNotReflectorEvent) {
		t.Errorf("expected ErrNotReflectorEvent, got %v", err)
	}
}

func TestDecodeUpdate_emptyPricesIsNoOp(t *testing.T) {
	prev, prevTS := decodeUpdateBody, decodeUpdateTimestamp
	defer func() { decodeUpdateBody, decodeUpdateTimestamp = prev, prevTS }()
	decodeUpdateTimestamp = func(_ string) (uint64, error) { return 0, nil }
	decodeUpdateBody = func(_ string) ([]PriceEntry, error) {
		return []PriceEntry{}, nil
	}

	e := &events.Event{
		Topic:      []string{TopicSymbolReflector, TopicSymbolUpdate, "ts"},
		ContractID: dexContractID,
	}
	updates, err := decodeUpdate(e, VariantDEX, DefaultDecimals, "", time.Now())
	if err != nil || updates != nil {
		t.Errorf("empty on-wire vector: got (%v, %v), want (nil, nil) no-op", updates, err)
	}
}

func TestDecodeUpdate_rejectsPriceVectorLargerThanStride(t *testing.T) {
	// Safety: a prices vector bigger than opIndexFanoutStride would
	// overflow the fanned-out OpIndex range into the next operation's
	// synthetic slot and cause PK collisions in oracle_updates.
	// Refuse loudly with ErrPriceVectorOverflow instead.
	prev, prevTS := decodeUpdateBody, decodeUpdateTimestamp
	defer func() { decodeUpdateBody, decodeUpdateTimestamp = prev, prevTS }()
	decodeUpdateTimestamp = func(_ string) (uint64, error) { return 0, nil }

	xlm := canonical.NativeAsset()
	oversized := make([]PriceEntry, opIndexFanoutStride+1)
	price := canonical.NewAmount(big.NewInt(1))
	for i := range oversized {
		oversized[i] = PriceEntry{Asset: xlm, Price: price}
	}
	decodeUpdateBody = func(_ string) ([]PriceEntry, error) {
		return oversized, nil
	}

	e := &events.Event{
		Topic:      []string{TopicSymbolReflector, TopicSymbolUpdate, "ts"},
		ContractID: dexContractID,
	}
	_, err := decodeUpdate(e, VariantDEX, DefaultDecimals, "", time.Now())
	if !errors.Is(err, ErrPriceVectorOverflow) {
		t.Errorf("expected ErrPriceVectorOverflow, got %v", err)
	}
}

func TestDecoder_NamesPerVariant(t *testing.T) {
	cases := []struct {
		variant  Variant
		wantName string
	}{
		{VariantDEX, SourceDEX},
		{VariantCEX, SourceCEX},
		{VariantFX, SourceFX},
	}
	for _, tc := range cases {
		t.Run(tc.wantName, func(t *testing.T) {
			d := NewDecoder(tc.variant, "Ccontract")
			if d.Name() != tc.wantName {
				t.Errorf("Name() = %q, want %q", d.Name(), tc.wantName)
			}
		})
	}
}

func TestUpdateEvent_sourceMatchesUpdate(t *testing.T) {
	// The UpdateEvent.Source() must return the same thing as the
	// contained OracleUpdate's Source field — otherwise metric
	// labels drift between what the event reports and what the
	// row persisted to oracle_updates carries.
	u := canonical.OracleUpdate{Source: SourceCEX}
	e := UpdateEvent{Update: u}
	if e.Source() != SourceCEX {
		t.Errorf("UpdateEvent.Source() = %q, want %q", e.Source(), SourceCEX)
	}
}

// oracle_updates carries ts in its primary key, so a decoder change that
// shifts the ts of an already-stored event makes a re-derive INSERT a second
// row instead of conflicting. These goldens pin the exact ts per input; a
// failure here means a ts-derivation change needs its own cleanup run (see
// "Re-deriving a timestamp" in docs/architecture/ingest-pipeline.md).
func TestDecodeUpdate_TimestampGolden(t *testing.T) {
	closedAt := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	ceil := closedAt.Add(24 * time.Hour) // literal: a change to the shared window must trip this

	for name, tc := range map[string]struct {
		topicMs uint64
		want    time.Time
	}{
		"topic ms wins (past)":       {1_745_123_456_000, time.UnixMilli(1_745_123_456_000)},
		"millisecond precision kept": {1_745_123_456_789, time.UnixMilli(1_745_123_456_789)},
		"zero clamps to close":       {0, closedAt},
		"pre-2001 floor clamps":      {999_999_999_999, closedAt},
		"at 2001 floor kept":         {1_000_000_000_000, time.UnixMilli(1_000_000_000_000)},
		"at close+24h kept":          {uint64(ceil.UnixMilli()), ceil},
		"close+24h+1ms clamps":       {uint64(ceil.UnixMilli()) + 1, closedAt},
		"u64 max clamps":             {^uint64(0), closedAt},
	} {
		t.Run(name, func(t *testing.T) {
			usd := xdr.ScSymbol("USD")
			body := encodeUpdateBody(t,
				[]xdr.ScVal{{Type: xdr.ScValTypeScvSymbol, Sym: &usd}},
				[]*big.Int{big.NewInt(100_000_000_000_000)})
			e := &events.Event{
				Topic:          []string{TopicSymbolReflector, TopicSymbolUpdate, encodeTimestampTopic(t, tc.topicMs)},
				Value:          body,
				ContractID:     adapterContract,
				Ledger:         52_000_000,
				TxHash:         "abc",
				LedgerClosedAt: closedAt.Format(time.RFC3339),
			}
			got, err := decodeUpdate(e, VariantCEX, DefaultDecimals, "GRELAYER", closedAt)
			if err != nil {
				t.Fatalf("decodeUpdate: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d updates, want 1", len(got))
			}
			if !got[0].Timestamp.Equal(tc.want) {
				t.Errorf("ts = %s (%d ms), want %s (%d ms)",
					got[0].Timestamp, got[0].Timestamp.UnixMilli(), tc.want, tc.want.UnixMilli())
			}
		})
	}
}

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

// ─── consumer.go ──────────────────────────────────────────────

func TestUpdateEvent_implementsConsumerEvent(t *testing.T) {
	ue := UpdateEvent{Update: canonical.OracleUpdate{Source: "reflector-dex"}}
	if got := ue.EventKind(); got != "reflector.update" {
		t.Errorf("EventKind() = %q, want \"reflector.update\"", got)
	}
	// Source is delegated to the contained Update.Source — exercises
	// the per-variant metric routing path (reflector-dex /
	// reflector-cex / reflector-fx).
	if got := ue.Source(); got != "reflector-dex" {
		t.Errorf("Source() = %q, want \"reflector-dex\"", got)
	}
	var _ consumer.Event = ue
}

// ─── dispatcher_adapter.go ────────────────────────────────────

const adapterContract = "CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN"

func TestDecoder_Name_perVariant(t *testing.T) {
	cases := []struct {
		variant Variant
		want    string
	}{
		{VariantDEX, "reflector-dex"},
		{VariantCEX, "reflector-cex"},
		{VariantFX, "reflector-fx"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			d := NewDecoder(tc.variant, adapterContract)
			if got := d.Name(); got != tc.want {
				t.Errorf("Name() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDecoder_Matches(t *testing.T) {
	d := NewDecoder(VariantDEX, adapterContract)

	good := events.Event{
		Topic:      []string{TopicSymbolReflector, TopicSymbolUpdate, "anything"},
		ContractID: adapterContract,
	}
	if !d.Matches(good) {
		t.Error("Matches(REFLECTOR:update from configured contract) = false, want true")
	}

	// Wrong contract — keeps DEX from picking up CEX events.
	wrongContract := events.Event{
		Topic:      []string{TopicSymbolReflector, TopicSymbolUpdate, "x"},
		ContractID: "CWRONG",
	}
	if d.Matches(wrongContract) {
		t.Error("Matches(wrong ContractID) = true, want false")
	}

	// Right contract but wrong topic.
	wrongTopic := events.Event{
		Topic:      []string{"AAAACwAAAAhTT1JPU1dBUAAAAAA="},
		ContractID: adapterContract,
	}
	if d.Matches(wrongTopic) {
		t.Error("Matches(wrong topic) = true, want false")
	}
}

func TestWithDecoderObserver_setsObserver(t *testing.T) {
	d := NewDecoder(VariantCEX, adapterContract,
		WithDecoderObserver("GRELAYER0000000000000000000000000000000000000000000000000"))
	if d.observer == "" {
		t.Error("WithDecoderObserver did not set observer field")
	}
}

func TestWithDecoderDecimals(t *testing.T) {
	for _, tc := range []struct{ opt, want uint8 }{{0, DefaultDecimals}, {7, 7}} {
		if got := NewDecoder(VariantFX, adapterContract, WithDecoderDecimals(tc.opt)).decimals; got != tc.want {
			t.Errorf("WithDecoderDecimals(%d): decimals = %d, want %d", tc.opt, got, tc.want)
		}
	}
}

func TestDecoder_Decode_emitsUpdatesForKnownSymbol(t *testing.T) {
	// Build a fixture with one fiat:USD entry — decodes via the
	// CEX/FX symbol path and surfaces as a single UpdateEvent.
	usd := xdr.ScSymbol("USD")
	symSv := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &usd}
	body := encodeUpdateBody(t, []xdr.ScVal{symSv}, []*big.Int{big.NewInt(100_000_000_000_000)})
	tsB64 := encodeTimestampTopic(t, 1_745_000_000_000)

	d := NewDecoder(VariantCEX, adapterContract,
		WithDecoderObserver("GRELAYER0000000000000000000000000000000000000000000000000"))
	out, err := d.Decode(events.Event{
		Topic:          []string{TopicSymbolReflector, TopicSymbolUpdate, tsB64},
		Value:          body,
		ContractID:     adapterContract,
		Ledger:         52_000_000,
		TxHash:         "abc",
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	ue, ok := out[0].(UpdateEvent)
	if !ok {
		t.Fatalf("expected UpdateEvent, got %T", out[0])
	}
	if ue.Update.Source != "reflector-cex" {
		t.Errorf("Update.Source = %q, want \"reflector-cex\"", ue.Update.Source)
	}
}

func TestDecoder_Decode_emptyUpdateDataIsNoOp(t *testing.T) {
	// A decode error here would count the event undecodable and blind the
	// ledger's completeness verdict; an empty batch has nothing to project.
	d := NewDecoder(VariantCEX, adapterContract)
	out, err := d.Decode(events.Event{
		Topic:          []string{TopicSymbolReflector, TopicSymbolUpdate, encodeTimestampTopic(t, 1_745_000_000_000)},
		Value:          encodeUpdateBody(t, nil, nil),
		ContractID:     adapterContract,
		Ledger:         52_000_000,
		TxHash:         "abc",
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	})
	if err != nil {
		t.Fatalf("Decode(empty update_data) = %v, want a nil-error no-op", err)
	}
	if len(out) != 0 {
		t.Fatalf("got %d events, want 0", len(out))
	}
}

func TestDecoder_Decode_malformedClosedAtFailsClosed(t *testing.T) {
	// LedgerClosedAt empty — decoder FAILS CLOSED (returns the error)
	// rather than substituting time.Now(). closedAt is the fallback
	// decodeUpdate uses when topic[2] is missing / out of its sanity
	// window, so a wall-clock value here would mis-timestamp the row
	// during a backfill replay. Matches the comet/blend/phoenix
	// siblings.
	usd := xdr.ScSymbol("USD")
	symSv := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &usd}
	body := encodeUpdateBody(t, []xdr.ScVal{symSv}, []*big.Int{big.NewInt(1_000_000_000_000)})
	tsB64 := encodeTimestampTopic(t, 1_745_000_000_000)

	d := NewDecoder(VariantCEX, adapterContract)
	out, err := d.Decode(events.Event{
		Topic:      []string{TopicSymbolReflector, TopicSymbolUpdate, tsB64},
		Value:      body,
		ContractID: adapterContract,
		// LedgerClosedAt deliberately empty.
	})
	if err == nil {
		t.Fatalf("Decode with empty LedgerClosedAt should error, got nil (out=%v)", out)
	}
	if out != nil {
		t.Errorf("expected nil events on error, got %v", out)
	}
}

func TestDecoder_Decode_topicTimestampWinsOverClosedAt(t *testing.T) {
	// With a present LedgerClosedAt, the in-window topic[2] oracle
	// timestamp is what lands on the OracleUpdate (the ledger close
	// time is only a fallback when topic[2] is missing / out of the
	// sanity window).
	usd := xdr.ScSymbol("USD")
	symSv := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &usd}
	body := encodeUpdateBody(t, []xdr.ScVal{symSv}, []*big.Int{big.NewInt(1_000_000_000_000)})
	const tsMs = 1_745_000_000_000
	tsB64 := encodeTimestampTopic(t, tsMs)

	d := NewDecoder(VariantCEX, adapterContract)
	out, err := d.Decode(events.Event{
		Topic:      []string{TopicSymbolReflector, TopicSymbolUpdate, tsB64},
		Value:      body,
		ContractID: adapterContract,
		// Ledger close a few seconds after the oracle stamp — within
		// the sanity window so topic[2] is accepted.
		LedgerClosedAt: time.UnixMilli(tsMs + 3000).UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	ue := out[0].(UpdateEvent)
	if ue.Update.Timestamp.UnixMilli() != tsMs {
		t.Errorf("Timestamp = %v, want topic[2]'s ms value", ue.Update.Timestamp)
	}
}

func TestDecoder_Decode_propagatesDecodeError(t *testing.T) {
	// Body is non-base64 — sdkDecodeUpdateBody returns an error
	// that decodeUpdate wraps; Decode must surface it.
	d := NewDecoder(VariantDEX, adapterContract)
	_, err := d.Decode(events.Event{
		Topic:          []string{TopicSymbolReflector, TopicSymbolUpdate, "garbagets"},
		Value:          "not-base64",
		ContractID:     adapterContract,
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	})
	if err == nil {
		t.Error("expected decode error, got nil")
	}
}

// quoteForVariant has a default-fallback branch for unknown Variant
// values (e.g. a future ADR-0014 oracle variant we haven't wired
// up). Pin: the fallback returns fiat:USD (every real Reflector
// oracle denominates in USD-equivalent) rather than zero — a
// zero-asset return would later break canonical.NewPair downstream.
//
// TestQuoteForVariant in source_test.go covers DEX/CEX/FX. This
// adds the default branch.

func TestQuoteForVariant_unknownFallsBackToUSD(t *testing.T) {
	const bogus Variant = 99
	usd, _ := canonical.NewFiatAsset("USD")
	got := quoteForVariant(bogus)
	if !got.Equal(usd) {
		t.Errorf("quoteForVariant(unknown) = %+v, want fiat:USD (default fallback)", got)
	}
}

// sdkDecodeUpdateTimestamp's parse-error branch — invalid base64 →
// surfaced as wrapped error so callers can drop the event without
// crashing.
func TestSdkDecodeUpdateTimestamp_invalidBase64(t *testing.T) {
	if _, err := sdkDecodeUpdateTimestamp("!!!not-base64!!!"); err == nil {
		t.Error("expected parse error for invalid base64, got nil")
	}
}

// sdkDecodeUpdateTimestamp wrong-kind path — a Symbol where U64 is
// expected. AsU64 must reject; otherwise the timestamp slot would
// silently be 0.
func TestSdkDecodeUpdateTimestamp_wrongKind(t *testing.T) {
	// Encode an ScSymbol where U64 is expected — AsU64 must reject.
	sym := xdr.ScSymbol("not-a-u64")
	sv := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
	raw, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	bad := base64.StdEncoding.EncodeToString(raw)
	if _, err := sdkDecodeUpdateTimestamp(bad); err == nil {
		t.Error("expected error decoding Symbol as U64, got nil")
	}
}
