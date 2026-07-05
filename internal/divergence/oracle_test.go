package divergence_test

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/StellarIndex/stellar-index/internal/cachekeys"
	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/divergence"
)

// fakeOracleReader is an OracleReader that returns a canned row and
// captures the arguments of the last call for mapping assertions.
type fakeOracleReader struct {
	row *canonical.OracleUpdate
	err error

	gotSource    string
	gotBaseKeys  []string
	gotQuoteKeys []string
}

func (f *fakeOracleReader) LatestOracleObservation(_ context.Context, source string, baseKeys, quoteKeys []string) (*canonical.OracleUpdate, error) {
	f.gotSource = source
	f.gotBaseKeys = baseKeys
	f.gotQuoteKeys = quoteKeys
	return f.row, f.err
}

func newOracleRef(t *testing.T, source string, reader divergence.OracleReader, maxAge time.Duration) *divergence.OracleReference {
	t.Helper()
	ref, err := divergence.NewOracleReference(divergence.OracleReferenceOptions{
		Source: source,
		Reader: reader,
		MaxAge: maxAge,
	})
	if err != nil {
		t.Fatalf("NewOracleReference: %v", err)
	}
	return ref
}

func oracleRow(t *testing.T, source, priceDec string, decimals uint8, ts time.Time) *canonical.OracleUpdate {
	t.Helper()
	raw, ok := new(big.Int).SetString(priceDec, 10)
	if !ok {
		t.Fatalf("bad price literal %q", priceDec)
	}
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("parse USD: %v", err)
	}
	return &canonical.OracleUpdate{
		Source:    source,
		Ledger:    1,
		TxHash:    "aa",
		Timestamp: ts,
		Asset:     canonical.NativeAsset(),
		Quote:     usd,
		Price:     canonical.NewAmount(raw),
		Decimals:  decimals,
	}
}

// TestNewOracleReference_RequiresSourceAndReader — misconfiguration
// fails loudly at construction.
func TestNewOracleReference_RequiresSourceAndReader(t *testing.T) {
	if _, err := divergence.NewOracleReference(divergence.OracleReferenceOptions{
		Reader: &fakeOracleReader{},
	}); err == nil {
		t.Error("expected error when Source is empty")
	}
	if _, err := divergence.NewOracleReference(divergence.OracleReferenceOptions{
		Source: divergence.OracleSourceBand,
	}); err == nil {
		t.Error("expected error when Reader is nil")
	}
}

// TestOracleReference_ScaleExactness verifies the raw-integer →
// float64 conversion is exact at every stored oracle scale,
// including values above 2^53 that would corrupt under a float64
// intermediate (ADR-0003). Expected values are computed via
// strconv.ParseFloat of the decimal string — both paths must round
// identically (nearest-even) or the big-int path lost precision.
func TestOracleReference_ScaleExactness(t *testing.T) {
	cases := []struct {
		name     string
		source   string
		price    string // raw integer, base 10
		decimals uint8
		expected string // decimal string of price / 10^decimals
	}{
		// Reflector stores at 14 decimals. Raw value > 2^53.
		{
			"reflector E14 above 2^53", divergence.OracleSourceReflectorCEX,
			"123456789012345678", 14, "1234.56789012345678",
		},
		// Band single-asset relayed rates are E9.
		{
			"band E9", divergence.OracleSourceBand,
			"4567890123", 9, "4.567890123",
		},
		// Redstone per-feed prices are E8.
		{
			"redstone E8", divergence.OracleSourceRedstone,
			"12345678", 8, "0.12345678",
		},
		// E18-class scale (Band's pair-rate scale upstream): 21
		// digits of significand, far above 2^53 — must stay exact
		// through the big.Rat path up to float64's own rounding.
		{
			"E18 above 2^53", divergence.OracleSourceBand,
			"1234567890123456789012", 18, "1234.567890123456789012",
		},
		// XLM-ish small price at 14 decimals.
		{
			"reflector E14 sub-dollar", divergence.OracleSourceReflectorDEX,
			"11072000000000", 14, "0.11072",
		},
	}
	now := time.Now().UTC()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &fakeOracleReader{row: oracleRow(t, tc.source, tc.price, tc.decimals, now)}
			ref := newOracleRef(t, tc.source, reader, time.Hour)

			got, err := ref.LookupPrice(context.Background(), xlmUSD(t), now)
			if err != nil {
				t.Fatalf("LookupPrice: %v", err)
			}
			want, err := strconv.ParseFloat(tc.expected, 64)
			if err != nil {
				t.Fatalf("ParseFloat(%q): %v", tc.expected, err)
			}
			if got != want {
				t.Errorf("price = %v, want %v (exact for %s at E%d)",
					got, want, tc.price, tc.decimals)
			}
		})
	}
}

// TestOracleReference_PairMapping_XLMDualIdentity — `native` and
// `crypto:XLM` are the same asset on two wire forms; the reference
// must query BOTH so a Reflector-CEX row published under crypto:XLM
// matches our on-chain native/fiat:USD pair (and vice versa). Every
// other asset maps to exactly its canonical string.
func TestOracleReference_PairMapping_XLMDualIdentity(t *testing.T) {
	now := time.Now().UTC()
	reader := &fakeOracleReader{row: oracleRow(t, divergence.OracleSourceReflectorCEX, "11000000000000", 14, now)}
	ref := newOracleRef(t, divergence.OracleSourceReflectorCEX, reader, time.Hour)

	// native base expands to include crypto:XLM.
	if _, err := ref.LookupPrice(context.Background(), xlmUSD(t), now); err != nil {
		t.Fatalf("LookupPrice: %v", err)
	}
	if reader.gotSource != divergence.OracleSourceReflectorCEX {
		t.Errorf("source = %q, want reflector-cex", reader.gotSource)
	}
	if got := strings.Join(reader.gotBaseKeys, ","); got != "native,crypto:XLM" {
		t.Errorf("base keys = %q, want native,crypto:XLM", got)
	}
	if got := strings.Join(reader.gotQuoteKeys, ","); got != "fiat:USD" {
		t.Errorf("quote keys = %q, want fiat:USD", got)
	}

	// crypto:XLM base expands to include native.
	xlm, err := canonical.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatalf("parse crypto:XLM: %v", err)
	}
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("parse USD: %v", err)
	}
	if _, err := ref.LookupPrice(context.Background(), canonical.Pair{Base: xlm, Quote: usd}, now); err != nil {
		t.Fatalf("LookupPrice crypto:XLM: %v", err)
	}
	if got := strings.Join(reader.gotBaseKeys, ","); got != "crypto:XLM,native" {
		t.Errorf("base keys = %q, want crypto:XLM,native", got)
	}

	// Non-XLM assets don't expand.
	eur, err := canonical.ParseAsset("fiat:EUR")
	if err != nil {
		t.Fatalf("parse EUR: %v", err)
	}
	if _, err := ref.LookupPrice(context.Background(), canonical.Pair{Base: eur, Quote: usd}, now); err != nil {
		t.Fatalf("LookupPrice EUR/USD: %v", err)
	}
	if got := strings.Join(reader.gotBaseKeys, ","); got != "fiat:EUR" {
		t.Errorf("base keys = %q, want fiat:EUR", got)
	}
}

// TestOracleReference_NoObservationIsUnsupported — an oracle that
// has never published the pair is an ErrAssetUnsupported (coverage
// information), not a transport failure.
func TestOracleReference_NoObservationIsUnsupported(t *testing.T) {
	reader := &fakeOracleReader{row: nil}
	ref := newOracleRef(t, divergence.OracleSourceBand, reader, time.Hour)
	_, err := ref.LookupPrice(context.Background(), xlmUSD(t), time.Now().UTC())
	if !errors.Is(err, divergence.ErrAssetUnsupported) {
		t.Errorf("err = %v, want ErrAssetUnsupported", err)
	}
}

// TestOracleReference_StaleObservationIsUnavailable — a frozen feed
// must read as "reference unavailable", never as agreement or
// divergence (CS-089 applied to served rows).
func TestOracleReference_StaleObservationIsUnavailable(t *testing.T) {
	now := time.Now().UTC()
	reader := &fakeOracleReader{
		row: oracleRow(t, divergence.OracleSourceReflectorCEX, "11000000000000", 14, now.Add(-45*time.Minute)),
	}
	ref := newOracleRef(t, divergence.OracleSourceReflectorCEX, reader, 30*time.Minute)
	_, err := ref.LookupPrice(context.Background(), xlmUSD(t), now)
	if !errors.Is(err, divergence.ErrPriceUnavailable) {
		t.Errorf("err = %v, want ErrPriceUnavailable", err)
	}

	// Just inside the ceiling passes.
	reader.row = oracleRow(t, divergence.OracleSourceReflectorCEX, "11000000000000", 14, now.Add(-29*time.Minute))
	if _, err := ref.LookupPrice(context.Background(), xlmUSD(t), now); err != nil {
		t.Errorf("fresh-enough observation rejected: %v", err)
	}
}

// TestOracleReference_ReaderErrorSurfaces — storage failures are
// generic transport-class failures (Result.Failures verbatim), not
// the unsupported/unavailable sentinels.
func TestOracleReference_ReaderErrorSurfaces(t *testing.T) {
	reader := &fakeOracleReader{err: errors.New("pg down")}
	ref := newOracleRef(t, divergence.OracleSourceRedstone, reader, time.Hour)
	_, err := ref.LookupPrice(context.Background(), xlmUSD(t), time.Now().UTC())
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, divergence.ErrAssetUnsupported) || errors.Is(err, divergence.ErrPriceUnavailable) {
		t.Errorf("reader error misclassified as sentinel: %v", err)
	}
}

// TestOracleReference_NonPositivePriceRejected — defensive: a
// zero/negative stored price never becomes a reference price.
func TestOracleReference_NonPositivePriceRejected(t *testing.T) {
	now := time.Now().UTC()
	reader := &fakeOracleReader{row: oracleRow(t, divergence.OracleSourceBand, "0", 9, now)}
	ref := newOracleRef(t, divergence.OracleSourceBand, reader, time.Hour)
	if _, err := ref.LookupPrice(context.Background(), xlmUSD(t), now); err == nil {
		t.Error("expected error for zero price")
	}
}

// TestRefreshPair_OnChainOracleReferences is the worker-cycle test:
// a Service wired with three on-chain oracle references (two fresh,
// one stale) refreshes a pair and the cached result + observation
// sink carry the per-reference outcomes under the oracle source
// labels — same observation schema as the HTTP references.
func TestRefreshPair_OnChainOracleReferences(t *testing.T) {
	now := time.Now().UTC()
	// reflector-cex: XLM at 0.11 (14 decimals), fresh.
	cexReader := &fakeOracleReader{
		row: oracleRow(t, divergence.OracleSourceReflectorCEX, "11000000000000", 14, now.Add(-time.Minute)),
	}
	// redstone: XLM at 0.11 (8 decimals), fresh.
	redstoneReader := &fakeOracleReader{
		row: oracleRow(t, divergence.OracleSourceRedstone, "11000000", 8, now.Add(-time.Minute)),
	}
	// band: stale beyond its ceiling.
	bandReader := &fakeOracleReader{
		row: oracleRow(t, divergence.OracleSourceBand, "110000000", 9, now.Add(-48*time.Hour)),
	}
	refs := []divergence.Reference{
		newOracleRef(t, divergence.OracleSourceReflectorCEX, cexReader, 30*time.Minute),
		newOracleRef(t, divergence.OracleSourceRedstone, redstoneReader, 26*time.Hour),
		newOracleRef(t, divergence.OracleSourceBand, bandReader, 26*time.Hour),
	}
	sink := &recordingObservationSink{}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{ObservationSink: sink})

	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 0.11, now); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}

	body, err := rdb.Get(context.Background(), cachekeys.Divergence(xlmUSD(t))).Bytes()
	if err != nil {
		t.Fatalf("redis get: %v", err)
	}
	var cached divergence.CachedResult
	if err := json.Unmarshal(body, &cached); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cached.SuccessCount != 2 {
		t.Errorf("SuccessCount = %d, want 2 (cex + redstone)", cached.SuccessCount)
	}
	if got := cached.Sources[divergence.OracleSourceReflectorCEX]; got != 0.11 {
		t.Errorf("reflector-cex price = %v, want 0.11", got)
	}
	if got := cached.Sources[divergence.OracleSourceRedstone]; got != 0.11 {
		t.Errorf("redstone price = %v, want 0.11", got)
	}
	if got := cached.Failures[divergence.OracleSourceBand]; got != "price_unavailable" {
		t.Errorf("band failure = %q, want price_unavailable", got)
	}
	if cached.WarningFired {
		t.Error("agreeing references must not fire the warning")
	}

	// Observation sink got one row per SUCCESSFUL reference, labeled
	// by oracle source.
	seen := map[string]bool{}
	for _, o := range sink.records {
		seen[o.Reference] = true
		if o.OurPrice != 0.11 {
			t.Errorf("observation %s OurPrice = %v, want 0.11", o.Reference, o.OurPrice)
		}
		if o.Firing {
			t.Errorf("observation %s unexpectedly firing", o.Reference)
		}
	}
	if !seen[divergence.OracleSourceReflectorCEX] || !seen[divergence.OracleSourceRedstone] {
		t.Errorf("sink references = %v, want reflector-cex + redstone", seen)
	}
	if seen[divergence.OracleSourceBand] {
		t.Error("stale band reference must not produce an observation row")
	}
}
