package projector

import (
	"context"
	"encoding/base64"
	"sync"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
)

// Real sushiswap_v3 lake bodies (internal/sources/sushiswap_v3/fixtures_test.go):
// the factory's pool_created for the XLM/USDC 0.30% pool (ledger
// 61,487,379) and a swap from the original pool WASM.
const (
	sushiPoolCreatedB64 = "AAAAEQAAAAEAAAAGAAAADwAAAANmZWUAAAAAAwAAC7gAAAAPAAAADHBvb2xfYWRkcmVzcwAAABIAAAABo6EfhoVFk5viOWrGgaJXOip0dVXyhCJjzAFNiInP4wMAAAAPAAAABnNlbmRlcgAAAAAAEgAAAAH2qKjDjWz71Dut10ZBTL+vn0NH3viK5Hy92gYqUawX0wAAAA8AAAAMdGlja19zcGFjaW5nAAAABAAAADwAAAAPAAAABnRva2VuMAAAAAAAEgAAAAEltPzYWa7C+mNIQ4xImzw8EMmLbSG+T9PLMMtolT75dwAAAA8AAAAGdG9rZW4xAAAAAAASAAAAAa3vzlmu5Slo92Bh1JTCUlt1ZZ+kKWpl9JnvKeVkd+SW"
	sushiSwapB64        = "AAAAEQAAAAEAAAAHAAAADwAAAAdhbW91bnQwAAAAAAoAAAAAAAAAAAAAAAAFuAFpAAAADwAAAAdhbW91bnQxAAAAAAr/////////////////CT5hAAAADwAAAAlsaXF1aWRpdHkAAAAAAAAJAAAAAAAAAAAAAAAAHZIo3QAAAA8AAAAJcmVjaXBpZW50AAAAAAAAEgAAAAAAAAAAxRy/OA51yJ4u3YL0mKNf2jKqkAy3kYfYMFIdzMphcBgAAAAPAAAABnNlbmRlcgAAAAAAEgAAAAAAAAAAxRy/OA51yJ4u3YL0mKNf2jKqkAy3kYfYMFIdzMphcBgAAAAPAAAADnNxcnRfcHJpY2VfeDk2AAAAAAALAAAAAAAAAAAAAAAAAAAAAAAAAABlKxxd8TMIn9sIRdQAAAAPAAAABHRpY2sAAAAE//+3dw=="
)

// prefilterStore is fakeStore with a StreamSorobanEvents that honours the
// contract-id prefilter the way the real SQL / ClickHouse reads do.
type prefilterStore struct {
	*fakeStore
	mu      sync.Mutex
	filters [][]string
}

func (s *prefilterStore) StreamSorobanEvents(ctx context.Context, from, to uint32,
	contractIDs, topics, exclude []string, fn func(row sorobanevents.Row) error,
) error {
	s.mu.Lock()
	s.filters = append(s.filters, append([]string(nil), contractIDs...))
	s.mu.Unlock()
	allowed := make(map[string]bool, len(contractIDs))
	for _, c := range contractIDs {
		allowed[c] = true
	}
	return s.fakeStore.StreamSorobanEvents(ctx, from, to, nil, topics, exclude,
		func(r sorobanevents.Row) error {
			if len(contractIDs) > 0 && !allowed[r.ContractID] {
				return nil
			}
			return fn(r)
		})
}

func mustDecodeB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// poolCreatedFor rewrites the real pool_created body to announce pool.
func poolCreatedFor(t *testing.T, pool string) []byte {
	t.Helper()
	var sv xdr.ScVal
	if err := sv.UnmarshalBinary(mustDecodeB64(t, sushiPoolCreatedB64)); err != nil {
		t.Fatal(err)
	}
	raw, err := strkey.Decode(strkey.VersionByteContract, pool)
	if err != nil {
		t.Fatal(err)
	}
	var cid xdr.ContractId
	copy(cid[:], raw)
	for i := range **sv.Map {
		e := &(**sv.Map)[i]
		if string(*e.Key.Sym) == "pool_address" {
			e.Val.Address.ContractId = &cid
		}
	}
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sushiRow(t *testing.T, ledger uint32, contract, topic0B64 string, body []byte) sorobanevents.Row {
	t.Helper()
	txHash := make([]byte, 32)
	txHash[0] = byte(ledger)
	return sorobanevents.Row{
		Ledger:          ledger,
		LedgerCloseTime: time.Unix(1_750_000_000+int64(ledger), 0).UTC(),
		TxHash:          txHash,
		ContractID:      contract,
		TopicCount:      1,
		Topic0XDR:       mustDecodeB64(t, topic0B64),
		BodyXDR:         body,
	}
}

// TestCycle_PrefilterTracksLiveFactorySeededGate pins #566: sushiswap_v3's
// gate grows LIVE from the factory's pool_created events, so the contract-id
// prefilter must follow it. A pool created at ledger 101 and traded at 102
// — both inside one projector window — must have its swap projected before
// the cursor passes 102. A boot-time snapshot filtered the swap out ahead of
// Matches and advanced the cursor over it.
func TestCycle_PrefilterTracksLiveFactorySeededGate(t *testing.T) {
	reg, err := BuildRegistry([]string{sushiswap_v3.SourceName}, config.OracleConfig{}, nil, nil)
	if err != nil || len(reg.Sources) != 1 {
		t.Fatalf("BuildRegistry: %v (%d sources)", err, len(reg.Sources))
	}
	src := reg.Sources[0]

	var newPoolRaw [32]byte
	newPoolRaw[0] = 0x5a
	newPool, err := strkey.Encode(strkey.VersionByteContract, newPoolRaw[:])
	if err != nil {
		t.Fatal(err)
	}
	store := &prefilterStore{fakeStore: &fakeStore{
		projectorCursor: 100, haveCursor: true, tipLedger: 105,
		rows: []sorobanevents.Row{
			sushiRow(t, 101, sushiswap_v3.MainnetFactory, sushiswap_v3.TopicSymbolPoolCreated, poolCreatedFor(t, newPool)),
			sushiRow(t, 102, newPool, sushiswap_v3.TopicSymbolSwap, mustDecodeB64(t, sushiSwapB64)),
		},
	}}
	var mu sync.Mutex
	var trades []uint32
	p := &Projector{
		store:  store,
		logger: discardLog(),
		sink: func(_ context.Context, ev consumer.Event) error {
			if te, ok := ev.(sushiswap_v3.TradeEvent); ok {
				mu.Lock()
				trades = append(trades, te.Trade.Ledger)
				mu.Unlock()
			}
			return nil
		},
	}
	widenedBefore := runsCount(t, src.Name, "gate_widened")
	window := uint32(BatchLimit)
	var tracker poisonTracker
	var wedge wedgeTracker
	for i := 0; i < 3 && store.cursor() < 105; i++ {
		p.cycleOneSource(context.Background(), src, &window, &tracker, &wedge, nil)
	}

	if got := store.cursor(); got != 105 {
		t.Fatalf("cursor = %d, want 105 (the window must still complete)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(trades) == 0 {
		t.Fatalf("the swap of pool %s (created at 101, traded at 102) was never projected; prefilters used: %v",
			newPool, store.filters)
	}
	for _, l := range trades {
		if l != 102 {
			t.Errorf("projected trade at ledger %d, want 102", l)
		}
	}
	if got := runsCount(t, src.Name, "gate_widened") - widenedBefore; got != 1 {
		t.Errorf("gate_widened cycles = %v, want 1 (one held re-read, then convergence)", got)
	}
}
