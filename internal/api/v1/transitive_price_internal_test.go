// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubPricer implements TransitivePricer with a canned answer.
type stubPricer struct {
	tp    timescale.TransitivePrice
	ok    bool
	err   error
	calls int
}

func (p *stubPricer) TransitiveUSDPrice(context.Context, string) (timescale.TransitivePrice, bool, error) {
	p.calls++
	return p.tp, p.ok, p.err
}

// The whole safety property of transitive pricing lives in this one
// function, so every arm of its gate is pinned here. A two-hop price
// inherits its weakest leg: serving one whose intermediate was never
// substance-checked would let a thin middle market reprice everything
// quoted against it — the manipulation the floors exist to stop, one hop
// removed.
func TestTransitivePriceFor_GateMatrix(t *testing.T) {
	const (
		assetID = "CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J"
		hopID   = "CBIJBDNZNF4X35BJ4FFZWCDBSCKOP5NB4PLG4SNENRMLAPYG4P5FM6VN"
	)
	asset, err := canonical.ParseAsset(assetID)
	if err != nil {
		t.Fatalf("parse asset: %v", err)
	}
	priced := timescale.TransitivePrice{PriceUSD: "7934.40", Hop: hopID, HopVolume24hUSD: "18755.68"}

	// Both legs allowed: near leg (asset,hop) and the hop standing on its
	// own against XLM — the same shape listingPriceAllowed checks.
	bothLegs := map[string]bool{
		assetID + "|" + hopID: true,
		hopID + "|native":     true,
	}

	tests := []struct {
		name      string
		pricer    TransitivePricer
		gate      PriceSubstanceGate
		wantPrice string
		wantOK    bool
	}{
		{
			name:   "no pricer wired — feature off, nothing served",
			pricer: nil,
			gate:   &stubListingGate{allow: bothLegs},
		},
		{
			name:   "resolver found no route",
			pricer: &stubPricer{ok: false},
			gate:   &stubListingGate{allow: bothLegs},
		},
		{
			name:   "resolver errored — never serve an unverified price",
			pricer: &stubPricer{err: errors.New("db down")},
			gate:   &stubListingGate{allow: bothLegs},
		},
		{
			name:   "empty price string is not a price",
			pricer: &stubPricer{tp: timescale.TransitivePrice{Hop: hopID}, ok: true},
			gate:   &stubListingGate{allow: bothLegs},
		},
		{
			name:   "unparseable hop cannot be gated, so cannot be trusted",
			pricer: &stubPricer{tp: timescale.TransitivePrice{PriceUSD: "1.23", Hop: "not-an-asset"}, ok: true},
			gate:   &stubListingGate{allow: bothLegs},
		},
		{
			// The dangerous one: with no gate wired we must refuse, not
			// fall through to serving. A price the gate never saw is
			// exactly what the gate exists to prevent.
			name:   "substance gate not wired — refuse rather than serve ungated",
			pricer: &stubPricer{tp: priced, ok: true},
			gate:   nil,
		},
		{
			name:   "near leg too thin — the asset/hop market is not trustworthy",
			pricer: &stubPricer{tp: priced, ok: true},
			gate:   &stubListingGate{allow: map[string]bool{hopID + "|native": true}},
		},
		{
			name:   "far leg too thin — the hop cannot stand on its own",
			pricer: &stubPricer{tp: priced, ok: true},
			gate:   &stubListingGate{allow: map[string]bool{assetID + "|" + hopID: true}},
		},
		{
			name:      "both legs clear — price is served",
			pricer:    &stubPricer{tp: priced, ok: true},
			gate:      &stubListingGate{allow: bothLegs},
			wantPrice: "7934.40",
			wantOK:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{transitive: tc.pricer, substance: tc.gate}
			got, ok := s.transitivePriceFor(context.Background(), asset, assetID)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (price=%q)", ok, tc.wantOK, got)
			}
			if got != tc.wantPrice {
				t.Errorf("price = %q, want %q", got, tc.wantPrice)
			}
		})
	}
}

// A nil pricer must not even reach the gate — the feature being off
// should cost nothing, not merely produce no output.
func TestTransitivePriceFor_NilPricerDoesNotConsultGate(t *testing.T) {
	gate := &stubListingGate{allow: map[string]bool{}}
	s := &Server{transitive: nil, substance: gate}
	asset, err := canonical.ParseAsset("CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.transitivePriceFor(context.Background(), asset, asset.String()); ok {
		t.Fatal("served a price with no pricer wired")
	}
}

// The resolver is consulted exactly once per call — it is a per-request
// SQL round trip on a serving path, not something to fan out.
func TestTransitivePriceFor_ResolverCalledOnce(t *testing.T) {
	const hopID = "CBIJBDNZNF4X35BJ4FFZWCDBSCKOP5NB4PLG4SNENRMLAPYG4P5FM6VN"
	assetID := "CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J"
	asset, err := canonical.ParseAsset(assetID)
	if err != nil {
		t.Fatal(err)
	}
	p := &stubPricer{tp: timescale.TransitivePrice{PriceUSD: "1.00", Hop: hopID}, ok: true}
	s := &Server{transitive: p, substance: &stubListingGate{allow: map[string]bool{
		assetID + "|" + hopID: true,
		hopID + "|native":     true,
	}}}
	if _, ok := s.transitivePriceFor(context.Background(), asset, assetID); !ok {
		t.Fatal("expected a served price")
	}
	if p.calls != 1 {
		t.Errorf("resolver calls = %d, want 1", p.calls)
	}
}

// REGRESSION (v0.46.0): the transitive fill must run when the asset has
// NO catalogue row at all.
//
// `classic_assets` requires a G-issuer (issuer_g_strkey NOT NULL), so a
// Soroban-native contract asset can never have a row — which is the
// entire premise of transitive pricing. The first implementation put the
// fill AFTER applyAssetRowToDetail's `sql.ErrNoRows` early return, so it
// was unreachable for exactly the assets it was built for. v0.46.0
// shipped and CAUP7 still served price_usd:null; nothing in the suite
// noticed, because every test fed it a row.
func TestApplyAssetRowToDetail_FillsTransitiveWhenNoCatalogueRow(t *testing.T) {
	const (
		assetID = "CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J"
		hopID   = "CBIJBDNZNF4X35BJ4FFZWCDBSCKOP5NB4PLG4SNENRMLAPYG4P5FM6VN"
	)
	asset, err := canonical.ParseAsset(assetID)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		transitive: &stubPricer{ok: true, tp: timescale.TransitivePrice{PriceUSD: "7934.40", Hop: hopID}},
		substance: &stubListingGate{allow: map[string]bool{
			assetID + "|" + hopID: true,
			hopID + "|native":     true,
		}},
	}

	var detail AssetDetail
	// The no-row case: GetAssetByAssetID returns sql.ErrNoRows.
	s.applyAssetRowToDetail(context.Background(), &detail, asset, timescale.AssetRow{}, sql.ErrNoRows, assetID)

	if detail.PriceUSD == nil {
		t.Fatal("no price filled — the transitive path is unreachable on the no-row branch")
	}
	if *detail.PriceUSD != "7934.40" {
		t.Errorf("price = %q, want 7934.40", *detail.PriceUSD)
	}
	if detail.PriceBasis != priceBasisTransitive {
		t.Errorf("price_basis = %q, want %q", detail.PriceBasis, priceBasisTransitive)
	}
}

// A REAL lookup error (not ErrNoRows) must still bail without pricing —
// we cannot tell whether a catalogue price exists, so inventing one from
// a hop would be guessing.
func TestApplyAssetRowToDetail_RealErrorDoesNotPrice(t *testing.T) {
	const assetID = "CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J"
	asset, err := canonical.ParseAsset(assetID)
	if err != nil {
		t.Fatal(err)
	}
	p := &stubPricer{ok: true, tp: timescale.TransitivePrice{PriceUSD: "1.00", Hop: "x"}}
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), transitive: p, substance: &stubListingGate{allow: map[string]bool{}}}

	var detail AssetDetail
	s.applyAssetRowToDetail(context.Background(), &detail, asset, timescale.AssetRow{}, errors.New("db exploded"), assetID)

	if detail.PriceUSD != nil {
		t.Fatal("priced despite an unexplained catalogue error")
	}
	if p.calls != 0 {
		t.Errorf("resolver calls = %d, want 0 — a real error must not fall through", p.calls)
	}
}
