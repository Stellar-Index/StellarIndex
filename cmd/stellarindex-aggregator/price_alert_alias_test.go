package main

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// pairKeyedAlertStore answers only for the pairs it holds, the way
// prices_1m does: `native`/fiat:USD has no rows because every venue
// quoting XLM in USD writes crypto:XLM.
type pairKeyedAlertStore struct {
	latest   map[string]timescale.Vwap1mRow
	errs     map[string]error
	trailing map[string][]timescale.Vwap1mRow
	asked    []string
}

func (s *pairKeyedAlertStore) LatestClosedVWAP1mForPair(_ context.Context, p canonical.Pair) (timescale.Vwap1mRow, error) {
	s.asked = append(s.asked, p.String())
	if err, ok := s.errs[p.String()]; ok {
		return timescale.Vwap1mRow{}, err
	}
	if row, ok := s.latest[p.String()]; ok {
		return row, nil
	}
	return timescale.Vwap1mRow{}, sql.ErrNoRows
}

func (s *pairKeyedAlertStore) RecentClosedVWAP1mCombined(_ context.Context, p canonical.Pair, _ int) ([]timescale.Vwap1mRow, error) {
	return s.trailing[p.String()], nil
}

func mustAlertPair(t *testing.T, base, quote canonical.Asset) canonical.Pair {
	t.Helper()
	p, err := canonical.NewPair(base, quote)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return p
}

func cryptoXLM(t *testing.T) canonical.Asset {
	t.Helper()
	a, err := canonical.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatalf("ParseAsset crypto:XLM: %v", err)
	}
	return a
}

// An alert on native/fiat:USD (the handler's own example id) must read
// the market recorded under crypto:XLM, as /v1/price does.
func TestPriceAlertReader_NativeUSDResolvesThroughCryptoXLMAlias(t *testing.T) {
	native, usd := alertUSDAssets(t)
	served := mustAlertPair(t, cryptoXLM(t), usd).String()
	store := &pairKeyedAlertStore{
		latest:   map[string]timescale.Vwap1mRow{served: alertRow(0, "1.01")},
		trailing: map[string][]timescale.Vwap1mRow{served: steadyAlertRows(12)},
	}
	reader := priceAlertVWAPReader{store: store, logger: discardLogger()}

	price, bucketClose, ok, err := reader.LatestVWAP(context.Background(), native, usd)
	if err != nil {
		t.Fatalf("LatestVWAP: %v", err)
	}
	if !ok {
		t.Fatalf("native/fiat:USD alert got no price (asked %v) — it can never fire "+
			"although crypto:XLM/fiat:USD holds a closed bucket", store.asked)
	}
	if price != "1.01" {
		t.Fatalf("price = %s, want the crypto:XLM/fiat:USD bucket 1.01", price)
	}
	if want := alertRow(0, "1.01").Bucket.Add(time.Minute); !bucketClose.Equal(want) {
		t.Fatalf("bucketClose = %v, want %v", bucketClose, want)
	}
	if store.asked[0] != mustAlertPair(t, native, usd).String() {
		t.Fatalf("first read = %s, want the literal pair first", store.asked[0])
	}
}

// The literal spelling keeps priority: an alias is read only when the
// literal pair has no closed bucket.
func TestPriceAlertReader_LiteralPairWinsOverAlias(t *testing.T) {
	native, usd := alertUSDAssets(t)
	literal := mustAlertPair(t, native, usd).String()
	alias := mustAlertPair(t, cryptoXLM(t), usd).String()
	store := &pairKeyedAlertStore{
		latest: map[string]timescale.Vwap1mRow{
			literal: alertRow(0, "1.01"),
			alias:   alertRow(0, "9.99"),
		},
		trailing: map[string][]timescale.Vwap1mRow{literal: steadyAlertRows(12)},
	}
	reader := priceAlertVWAPReader{store: store, logger: discardLogger()}
	price, _, ok, err := reader.LatestVWAP(context.Background(), native, usd)
	if err != nil || !ok || price != "1.01" {
		t.Fatalf("got price=%q ok=%v err=%v, want the literal pair's 1.01", price, ok, err)
	}
}

// A broken read is not "no market here": the walk must surface it rather
// than fall through to another spelling's (possibly thinner) book.
func TestPriceAlertReader_ReadErrorEndsAliasWalk(t *testing.T) {
	native, usd := alertUSDAssets(t)
	literal := mustAlertPair(t, native, usd).String()
	alias := mustAlertPair(t, cryptoXLM(t), usd).String()
	boom := errors.New("planning error")
	store := &pairKeyedAlertStore{
		latest: map[string]timescale.Vwap1mRow{alias: alertRow(0, "1.01")},
		errs:   map[string]error{literal: boom},
	}
	reader := priceAlertVWAPReader{store: store, logger: discardLogger()}
	_, _, ok, err := reader.LatestVWAP(context.Background(), native, usd)
	if !errors.Is(err, boom) || ok {
		t.Fatalf("got ok=%v err=%v, want the literal read's error", ok, err)
	}
}

// A price found under an alias spelling is gated exactly as the literal:
// a flagged issuer on the quote leg withholds it.
func TestPriceAlertReader_AliasServedPriceStillWithheld(t *testing.T) {
	flagged, err := canonical.NewClassicAsset("RIO", alertScamIssuer)
	if err != nil {
		t.Fatalf("build flagged classic: %v", err)
	}
	alias := mustAlertPair(t, cryptoXLM(t), flagged).String()
	store := &pairKeyedAlertStore{
		latest:   map[string]timescale.Vwap1mRow{alias: alertRow(0, "1.01")},
		trailing: map[string][]timescale.Vwap1mRow{alias: steadyAlertRows(12)},
	}
	dir := &alertScamDirectory{flagged: map[string]bool{alertScamIssuer: true}}
	reader := priceAlertVWAPReader{
		store:  store,
		logger: discardLogger(),
		gate:   pricingguard.Gate{Scam: pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{})},
	}
	price, _, ok, err := reader.LatestVWAP(context.Background(), canonical.NativeAsset(), flagged)
	if err != nil {
		t.Fatalf("LatestVWAP: %v", err)
	}
	if ok {
		t.Fatalf("alias-served price %q delivered for a directory-flagged issuer", price)
	}
	if len(store.asked) < 2 {
		t.Fatalf("asked %v — the walk never reached the alias bucket, so the gate went untested", store.asked)
	}
}
