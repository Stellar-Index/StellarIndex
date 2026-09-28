package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestRedisConfidenceLooker_ServesEveryCachedFactor — the adapter copies
// confidence.Factors into v1.ConfidenceFactors field by field, so a field
// the aggregator caches but the adapter forgets never reaches the wire.
// Decoding both sides by JSON name makes any such omission fail here.
func TestRedisConfidenceLooker_ServesEveryCachedFactor(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	base, err := canonical.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatal(err)
	}
	quote, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatal(err)
	}
	// Every flag set and every value distinct from zero, so a dropped
	// field cannot pass by coinciding with its zero value.
	score := confidence.Compute(confidence.Inputs{
		ZScore:                     0.3,
		SourceCount:                3,
		SourceClassCount:           2,
		LiquidityUSD:               250_000,
		CrossOracleDivergencePct:   0.4,
		CrossOracleAgreementCount:  2,
		TriangulationChecked:       true,
		TriangulationDivergencePct: 0.5,
		BaselineAgeDays:            5,
	}, confidence.DefaultWeights())
	body, err := json.Marshal(score)
	if err != nil {
		t.Fatal(err)
	}
	mr.Set(cachekeys.Confidence(base, quote, time.Minute).String(), string(body))

	got, ok, err := redisConfidenceLooker{rdb: rdb}.LookupConfidence(context.Background(), base, quote, time.Minute)
	if err != nil || !ok {
		t.Fatalf("LookupConfidence = (ok=%v, err=%v), want a hit", ok, err)
	}
	if got.Confidence != score.Confidence {
		t.Errorf("confidence = %v, want %v", got.Confidence, score.Confidence)
	}
	if want, served := byJSONName(t, score.Factors), byJSONName(t, got.Factors); !reflect.DeepEqual(served, want) {
		t.Errorf("served confidence_factors differ from the cached factors:\n served %v\n cached %v", served, want)
	}
}

func byJSONName(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
