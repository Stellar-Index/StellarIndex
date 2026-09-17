// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// filteringSep1Reader serves canned bound entries THROUGH the caller's
// keep filter, the way the production scan does
// (timescale.boundSep1CurrenciesFromPayload counts a refused entry as
// EntriesFiltered and never materialises it). The sibling test calls
// admitClassicCandidates directly and so never meets the filter; this
// reader is what lets a test meet it.
type filteringSep1Reader struct {
	bound []timescale.Sep1BoundCurrency
}

func (r *filteringSep1Reader) GetIssuerSep1Cached(context.Context, string) (*timescale.IssuerSep1Cached, error) {
	return nil, nil
}

func (r *filteringSep1Reader) BoundSep1Currencies(
	_ context.Context, keep timescale.Sep1CurrencyFilter,
) ([]timescale.Sep1BoundCurrency, timescale.Sep1BoundCensus, error) {
	census := timescale.Sep1BoundCensus{IssuersWithHomeDomain: 1, IssuersWithPayload: 1, IssuersDeclaring: 1}
	out := make([]timescale.Sep1BoundCurrency, 0, len(r.bound))
	for _, c := range r.bound {
		census.Entries++
		census.EntriesBound++
		if keep != nil && !keep(c) {
			census.EntriesFiltered++
			continue
		}
		census.EntriesKept++
		out = append(out, c)
	}
	return out, census, nil
}

// fixedDirectory is the curated directory over a fixed entry map.
type fixedDirectory map[string]timescale.DirectoryEntry

func (d fixedDirectory) DirectoryEntryByAddress(_ context.Context, address string) (timescale.DirectoryEntry, bool, error) {
	e, ok := d[address]
	return e, ok, nil
}

func (d fixedDirectory) DirectoryEntriesByAddresses(_ context.Context, addresses []string) (map[string]timescale.DirectoryEntry, error) {
	out := map[string]timescale.DirectoryEntry{}
	for _, a := range addresses {
		if e, ok := d[a]; ok {
			out[a] = e
		}
	}
	return out, nil
}

// The 2026-09-17 finding: the sibling arm and the ISIN arm both shipped
// green and had zero live effect, because the scan's pre-filter read the
// code and the anchor TYPE only. A Franklin-shaped declaration — code no
// oracle prices, type `other`, a well-formed ISIN in anchor_asset — was
// counted as EntriesFiltered before either arm ran. This drives the
// production filter through the production build and asserts the row
// the two arms exist for is admitted, with the recognition the sibling
// arm publishes.
func TestBuildRWAClassicMembership_FranklinShapedCandidatePassesThePreFilter(t *testing.T) {
	const (
		listed   = "GBHNGLLIE3KWGKCHIKMHJ5HVZHYIK7WTBE4QF5PLAKL4CJGSEU7HZIW5"
		unlisted = "GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP"
		domain   = "www.franklintempleton.com"
	)
	s := &Server{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		sep1Cache: &filteringSep1Reader{bound: []timescale.Sep1BoundCurrency{
			{Code: "BENJI", Issuer: listed, HomeDomain: domain, AnchorAssetType: "other", AnchorAsset: "FOBXX"},
			{Code: "gBENJI", Issuer: unlisted, HomeDomain: domain, AnchorAssetType: "other", AnchorAsset: "LU2900381208"},
			// The population the filter exists to drop, still dropped.
			{Code: "MEME", Issuer: unlisted, HomeDomain: domain, AnchorAssetType: "nft", AnchorAsset: ""},
			{Code: "NOPE", Issuer: unlisted, HomeDomain: domain, AnchorAssetType: "other", AnchorAsset: "LU2900381209"},
		}},
		directory: fixedDirectory{
			listed: {Name: "Franklin Templeton", Tags: []string{"issuer"}},
		},
	}
	out := s.buildRWAClassicMembership(context.Background())
	if !out.available {
		t.Fatal("membership unavailable")
	}

	got := map[string]rwaMember{}
	for _, m := range out.members {
		got[m.code] = m
	}
	m, ok := got["gBENJI"]
	if !ok {
		t.Fatalf("gBENJI not admitted; members = %v, refusals = %v", codesOf(out.members), out.refusals)
	}
	if m.issuer != unlisted || m.basis != rwa.BasisSep1ISIN || m.recognition != rwa.RecognitionDomainSibling {
		t.Errorf("gBENJI = %+v, want issuer %s, basis %s, recognition %s",
			m, unlisted, rwa.BasisSep1ISIN, rwa.RecognitionDomainSibling)
	}
	if m.anchorAsset != "LU2900381208" {
		t.Errorf("gBENJI anchor_asset = %q, want the declared ISIN", m.anchorAsset)
	}
	if b, ok := got["BENJI"]; !ok || b.recognition != rwa.RecognitionDirectory {
		t.Errorf("BENJI = %+v, want admitted on the direct directory arm", b)
	}
	for _, code := range []string{"MEME", "NOPE"} {
		if _, ok := got[code]; ok {
			t.Errorf("%s admitted: no class, no oracle code, no valid ISIN", code)
		}
	}
	// The filter still drops what it should, and only that: the NFT
	// entry and the malformed ISIN, counted under requirement 4.
	if out.census.EntriesFiltered != 2 || out.refusals[rwa.RejectNoInstrumentClaim] != 2 {
		t.Errorf("EntriesFiltered = %d, refusals[%s] = %d, want 2 and 2",
			out.census.EntriesFiltered, rwa.RejectNoInstrumentClaim, out.refusals[rwa.RejectNoInstrumentClaim])
	}
}

func codesOf(members []rwaMember) []string {
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, m.code)
	}
	return out
}
