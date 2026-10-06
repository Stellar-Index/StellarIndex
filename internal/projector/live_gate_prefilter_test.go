package projector

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/sources/spectra"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
)

// Real sushiswap_v3 lake bodies (internal/sources/sushiswap_v3/fixtures_test.go):
// the factory's pool_created for the XLM/USDC 0.30% pool (ledger
// 61,487,379) and a swap from the original pool WASM.
const (
	sushiPoolCreatedB64 = "AAAAEQAAAAEAAAAGAAAADwAAAANmZWUAAAAAAwAAC7gAAAAPAAAADHBvb2xfYWRkcmVzcwAAABIAAAABo6EfhoVFk5viOWrGgaJXOip0dVXyhCJjzAFNiInP4wMAAAAPAAAABnNlbmRlcgAAAAAAEgAAAAH2qKjDjWz71Dut10ZBTL+vn0NH3viK5Hy92gYqUawX0wAAAA8AAAAMdGlja19zcGFjaW5nAAAABAAAADwAAAAPAAAABnRva2VuMAAAAAAAEgAAAAEltPzYWa7C+mNIQ4xImzw8EMmLbSG+T9PLMMtolT75dwAAAA8AAAAGdG9rZW4xAAAAAAASAAAAAa3vzlmu5Slo92Bh1JTCUlt1ZZ+kKWpl9JnvKeVkd+SW"
	sushiSwapB64        = "AAAAEQAAAAEAAAAHAAAADwAAAAdhbW91bnQwAAAAAAoAAAAAAAAAAAAAAAAFuAFpAAAADwAAAAdhbW91bnQxAAAAAAr/////////////////CT5hAAAADwAAAAlsaXF1aWRpdHkAAAAAAAAJAAAAAAAAAAAAAAAAHZIo3QAAAA8AAAAJcmVjaXBpZW50AAAAAAAAEgAAAAAAAAAAxRy/OA51yJ4u3YL0mKNf2jKqkAy3kYfYMFIdzMphcBgAAAAPAAAABnNlbmRlcgAAAAAAEgAAAAAAAAAAxRy/OA51yJ4u3YL0mKNf2jKqkAy3kYfYMFIdzMphcBgAAAAPAAAADnNxcnRfcHJpY2VfeDk2AAAAAAALAAAAAAAAAAAAAAAAAAAAAAAAAABlKxxd8TMIn9sIRdQAAAAPAAAABHRpY2sAAAAE//+3dw=="
)

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
	return withContractField(t, sushiPoolCreatedB64, "pool_address", pool)
}

// withContractField rewrites the contract-address field of a real map body.
func withContractField(t *testing.T, bodyB64, field, contract string) []byte {
	t.Helper()
	var sv xdr.ScVal
	if err := sv.UnmarshalBinary(mustDecodeB64(t, bodyB64)); err != nil {
		t.Fatal(err)
	}
	raw, err := strkey.Decode(strkey.VersionByteContract, contract)
	if err != nil {
		t.Fatal(err)
	}
	var cid xdr.ContractId
	copy(cid[:], raw)
	for i := range **sv.Map {
		e := &(**sv.Map)[i]
		if string(*e.Key.Sym) == field {
			e.Val.Address.ContractId = &cid
		}
	}
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sushiEvent(ledger uint32, contract, topic0B64 string, body []byte) events.Event {
	txHash := make([]byte, 32)
	txHash[0] = byte(ledger)
	return events.Event{
		Type:           "contract",
		Ledger:         ledger,
		LedgerClosedAt: time.Unix(1_750_000_000+int64(ledger), 0).UTC().Format(time.RFC3339),
		ContractID:     contract,
		TxHash:         hex.EncodeToString(txHash),
		Topic:          []string{topic0B64},
		Value:          base64.StdEncoding.EncodeToString(body),
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
	store := &fakeStore{projectorCursor: 100, haveCursor: true, tipLedger: 105}
	lakeEvs := &fakeEvents{
		evs: []events.Event{
			sushiEvent(101, sushiswap_v3.MainnetFactory, sushiswap_v3.TopicSymbolPoolCreated, poolCreatedFor(t, newPool)),
			sushiEvent(102, newPool, sushiswap_v3.TopicSymbolSwap, mustDecodeB64(t, sushiSwapB64)),
		},
	}
	var mu sync.Mutex
	var trades []uint32
	p, lake := newLakeEventsProjector(store, lakeEvs, func(_ context.Context, ev consumer.Event) error {
		if te, ok := ev.(sushiswap_v3.TradeEvent); ok {
			mu.Lock()
			trades = append(trades, te.Trade.Ledger)
			mu.Unlock()
		}
		return nil
	})
	widenedBefore := runsCount(t, src.Name, "gate_widened")
	window := uint32(BatchLimit)
	var tracker poisonTracker
	var wedge wedgeTracker
	for i := 0; i < 3 && store.cursor() < 105; i++ {
		p.cycleOneSource(context.Background(), src, &window, &tracker, &wedge, lake)
	}

	if got := store.cursor(); got != 105 {
		t.Fatalf("cursor = %d, want 105 (the window must still complete)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(trades) == 0 {
		t.Fatalf("the swap of pool %s (created at 101, traded at 102) was never projected; prefilters used: %v",
			newPool, lakeEvs.filters)
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

// Real Spectra mainnet bodies (test/fixtures/spectra): the factory's
// pt_deployed and the PT's yt_deployed from the USDC market's creation
// transaction (ledger 63,782,624), and a YT transfer.
const (
	spectraPTDeployedTopic = "AAAADwAAAAtwdF9kZXBsb3llZAA="
	spectraPTDeployedB64   = "AAAAEQAAAAEAAAAEAAAADwAAAAhkZXBsb3llcgAAABIAAAAAAAAAAJovmvVGl4o2X6sTeIxd+eulvvp97zlWCE+aU/YQs1qKAAAADwAAAAhkdXJhdGlvbgAAAAUAAAAAAHanAAAAAA8AAAADaWJ0AAAAABIAAAABYz4ToD62ZkYI+fx4W7w7BsVwWtudoRG9cexHSjgMMiYAAAAPAAAAAnB0AAAAAAASAAAAAQDo9LzZpQzhJJHRVZshYdh90qlkgkOVQqBU6kJZZAXa"
	spectraYTDeployedTopic = "AAAADwAAAAt5dF9kZXBsb3llZAA="
	spectraYTDeployedB64   = "AAAAEQAAAAEAAAABAAAADwAAAAdhZGRyZXNzAAAAABIAAAABRwyW2P77E+3wW1XAoc745pxzMLK7gZRZ+RC+jA7Xffk="
	spectraTransferB64     = "AAAACgAAAAAAAAAAAAAAAAAAnEA="
)

var spectraTransferTopics = []string{
	"AAAADwAAAAh0cmFuc2Zlcg==",
	"AAAAEgAAAAAAAAAAmi+a9UaXijZfqxN4jF3566W++n3vOVYIT5pT9hCzWoo=",
	"AAAAEgAAAAGU1wln9BCzS8bGbMh5Ij2Fof+uEjsVheKhflTNzWyFnA==",
}

func spectraEvent(ledger uint32, eventIndex int, contract string, topicsB64 []string, body []byte) events.Event {
	ev := sushiEvent(ledger, contract, topicsB64[0], body)
	ev.EventIndex = eventIndex
	ev.Topic = slices.Clone(topicsB64)
	return ev
}

func testContract(t *testing.T, seed byte) string {
	t.Helper()
	var raw [32]byte
	raw[0] = seed
	c, err := strkey.Encode(strkey.VersionByteContract, raw[:])
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestCycle_PrefilterTracksSpectraSecondHop is the two-hop form of the test
// above. Spectra's gate grows twice in a market's creation transaction: the
// factory's pt_deployed admits the PT, then the PT's yt_deployed admits the
// YT. The PT's yt_deployed is emitted BEFORE the factory's pt_deployed, so
// the first read cannot see it; each widening must hold the window for a
// re-read, or the YT and its transfer one ledger later are lost.
func TestCycle_PrefilterTracksSpectraSecondHop(t *testing.T) {
	reg, err := BuildRegistry([]string{spectra.SourceName}, config.OracleConfig{}, nil, nil)
	if err != nil || len(reg.Sources) != 1 {
		t.Fatalf("BuildRegistry: %v (%d sources)", err, len(reg.Sources))
	}
	src := reg.Sources[0]

	pt, yt := testContract(t, 0x6a), testContract(t, 0x6b)
	store := &fakeStore{projectorCursor: 100, haveCursor: true, tipLedger: 105}
	lakeEvs := &fakeEvents{
		evs: []events.Event{
			spectraEvent(101, 4, pt, []string{spectraYTDeployedTopic},
				withContractField(t, spectraYTDeployedB64, "address", yt)),
			spectraEvent(101, 6, spectra.MainnetFactory, []string{spectraPTDeployedTopic},
				withContractField(t, spectraPTDeployedB64, "pt", pt)),
			spectraEvent(102, 2, yt, spectraTransferTopics, mustDecodeB64(t, spectraTransferB64)),
		},
	}
	var mu sync.Mutex
	projected := map[string]uint32{}
	p, lake := newLakeEventsProjector(store, lakeEvs, func(_ context.Context, ev consumer.Event) error {
		if se, ok := ev.(spectra.Event); ok {
			mu.Lock()
			projected[se.Kind+"@"+se.ContractID] = se.Ledger
			mu.Unlock()
		}
		return nil
	})
	widenedBefore := runsCount(t, src.Name, "gate_widened")
	window := uint32(BatchLimit)
	var tracker poisonTracker
	var wedge wedgeTracker
	for i := 0; i < 5 && store.cursor() < 105; i++ {
		p.cycleOneSource(context.Background(), src, &window, &tracker, &wedge, lake)
	}

	if got := store.cursor(); got != 105 {
		t.Fatalf("cursor = %d, want 105 (the window must still complete)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	want := map[string]uint32{
		spectra.EventPTDeployed + "@" + spectra.MainnetFactory: 101,
		spectra.EventYTDeployed + "@" + pt:                     101,
		spectra.EventTransfer + "@" + yt:                       102,
	}
	for k, l := range want {
		if got, ok := projected[k]; !ok || got != l {
			t.Errorf("%s projected at %d (present=%v), want ledger %d; prefilters used: %v",
				k, got, ok, l, lakeEvs.filters)
		}
	}
	if len(projected) != len(want) {
		t.Errorf("projected %v, want exactly %v", projected, want)
	}
	if got := runsCount(t, src.Name, "gate_widened") - widenedBefore; got != 2 {
		t.Errorf("gate_widened cycles = %v, want 2 (one held re-read per hop, then convergence)", got)
	}
}
