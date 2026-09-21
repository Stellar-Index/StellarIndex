package streampublish_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	"github.com/Stellar-Index/StellarIndex/internal/api/streampublish"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeDecimalsReader satisfies v1.NonstandardDecimalsReader with a fixed
// set of confirmed non-7-decimal assets, no database required.
type fakeDecimalsReader struct {
	rows []timescale.NonstandardDecimalsAsset
}

func (r *fakeDecimalsReader) LoadNonstandardDecimalsAssets(context.Context) ([]timescale.NonstandardDecimalsAsset, error) {
	return r.rows, nil
}

// TestPublisher_NormalizesNonstandardDecimals proves the SSE closed-bucket
// producer applies the SAME dex-nonstandard-decimals correction
// /v1/price applies before serving (RLT-353): reader.LatestPrice returns
// the RAW closed-1m ratio, and without normalization the wire payload
// would carry that raw (wrong by 10^11) value instead of the true price.
//
// Golden shape mirrors TestPriceTip_NonstandardDecimals_Normalizes: an
// 18dp Soroban base leg against a 7dp quote, ohlcPriceDigits=10 fixed
// formatting.
func TestPublisher_NormalizesNonstandardDecimals(t *testing.T) {
	sorobanContract := "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO" // gitleaks:allow — public Stellar contract id, not a secret
	decimals := v1.NewNonstandardDecimalsCache(&fakeDecimalsReader{
		rows: []timescale.NonstandardDecimalsAsset{{Asset: sorobanContract, Decimals: 18}},
	}, nil)
	if err := decimals.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	hub := streaming.NewHub(0)
	reader := &fakeReader{}
	asset := mustParse(t, sorobanContract)
	quote := mustParse(t, "fiat:USD")
	topic := v1.PriceStreamTopic(asset, quote, 60)

	bucket := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	reader.SetSnapshot(asset, quote, v1.PriceSnapshot{
		AssetID: sorobanContract, Quote: "fiat:USD", Price: "0.00000000005",
		PriceType: "vwap", ObservedAt: v1.WireTime(bucket), WindowSeconds: 60,
	})

	pub := streampublish.New(hub, reader, time.Second, nil, streampublish.Options{Decimals: decimals})

	ch, cancel := hub.Subscribe([]string{topic}, "")
	defer cancel()

	ctx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()

	done := make(chan struct{})
	go func() {
		_ = pub.Run(ctx, []canonical.Pair{{Base: asset, Quote: quote}})
		close(done)
	}()

	select {
	case ev := <-ch:
		var payload struct {
			Data v1.PriceSnapshot `json:"data"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		// Raw ratio 5e-11 scaled by 10^(18-7) = 5.0000000000, ohlcPriceDigits=10.
		// Unnormalized, the wire would carry the raw "0.00000000005".
		if payload.Data.Price != "5.0000000000" {
			t.Errorf("payload Price = %q, want normalized \"5.0000000000\"", payload.Data.Price)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event received within 2s of publisher start")
	}

	cancelRun()
	<-done
}
