package v1_test

import (
	"context"
	"encoding/json"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// togglingAquariusReader alternates the single pool's reserve between
// two well-known amounts on every read, so consecutive Refresh() cycles
// publish two DIFFERENT (and independently verifiable) tvl_usd figures
// rather than merely two different as_of stamps that could collide at
// RFC3339's second resolution.
type togglingAquariusReader struct {
	n atomic.Int64
}

func (r *togglingAquariusReader) LatestAquariusReserves(context.Context, int) ([]timescale.AquariusPoolReserve, error) {
	n := r.n.Add(1)
	// 400_000_000 raw units @ $0.25 = $10.00; 800_000_000 = $20.00.
	raw := int64(400_000_000)
	if n%2 == 0 {
		raw = 800_000_000
	}
	return []timescale.AquariusPoolReserve{{
		ContractID: "CBQDHNBFBZYE4MECPHNQCLM7F5FRZ4R7HZWQZXAK7NZYYUR3ILWSKDMV",
		ObservedAt: time.Now(),
		Ledger:     63_000_000,
		Legs: []timescale.AquariusReserveLeg{
			{TokenIndex: 0, Token: canonical.XLMSacContractID, Reserve: canonical.NewAmount(big.NewInt(raw))},
		},
	}}, nil
}

// TestHandleProtocolsList_TVLJoinIsAtomicAcrossRefresh is the RLT-235
// regression: GET /v1/protocols joins the per-protocol tvl block and
// the headline tvl_total from the DEX TVL cache. Both must come from
// the SAME refresh cycle. Serving them via two independent
// Snapshot()/Total() reads lets a concurrent Refresh() land between
// them, pairing one cycle's per-protocol figure with a different
// cycle's total — here made observable because the toggling reader
// makes each cycle's aquarius figure exactly $10.00 or $20.00, so a
// mismatch shows up as tvl_total.tvl_usd disagreeing with the
// aquarius row it names in tvl_total.protocols.
func TestHandleProtocolsList_TVLJoinIsAtomicAcrossRefresh(t *testing.T) {
	reader := &togglingAquariusReader{}
	cache := v1.NewDEXTVLCache(v1.DEXTVLSources{
		AquariusReserves: reader,
		Pricer:           stubTVLPricerT{},
	})
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("seed refresh: %v", err)
	}
	srv := v1.New(v1.Options{DEXTVL: cache})
	ts := httpTestServer(t, srv)

	const iterations = 400
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Hammer Refresh() concurrently so the cache alternates generation
	// as fast as possible while the reader loop below is mid-request.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = cache.Refresh(context.Background())
			}
		}
	}()

	var mismatches int
	for i := 0; i < iterations; i++ {
		resp := mustGet(t, ts.URL+"/v1/protocols")
		var env struct {
			Data v1.ProtocolsView `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		resp.Body.Close()
		if env.Data.TVLTotal == nil {
			continue
		}
		aq := protocolRow(t, env.Data.Protocols, "aquarius")
		included := false
		for _, name := range env.Data.TVLTotal.Protocols {
			if name == "aquarius" {
				included = true
			}
		}
		if !included || aq.TVL == nil {
			continue
		}
		// aquarius is the only protocol the toggling reader feeds, so
		// the reconciled total must equal that row's own figure
		// exactly — any disagreement means the response paired a
		// per-protocol row from one refresh cycle with a total from
		// another.
		if aq.TVL.TVLUSD != env.Data.TVLTotal.TVLUSD {
			mismatches++
		}
	}

	close(stop)
	wg.Wait()

	if mismatches > 0 {
		t.Errorf("tvl_total.tvl_usd disagreed with the aquarius row it names in %d/%d requests — "+
			"protocols[].tvl and tvl_total were read from different refresh cycles",
			mismatches, iterations)
	}
}
