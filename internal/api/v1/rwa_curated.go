// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The curated arm on the read path. The RULE is in internal/rwa/curated.go;
// this file is where the curator's rows become served rows, where they
// are kept apart from the verified set, and what the response says when
// the curator is not answering.

// rwaCuratorDune is the first and only curator today.
const rwaCuratorDune = "dune:stellar"

// RWAReferenceCuratorPrice is the reference provenance a curated row
// carries: the curator's own uploaded price per token. Weaker than a
// listing price — a listing platform at least aggregates venues; an
// uploaded price is whatever the curator typed.
const RWAReferenceCuratorPrice = "curator_uploaded_price"

// RWACuratedDirectoryReader is the seam the curated arm reads through.
// *timescale.Store satisfies it. Optional: a deployment without it
// serves no curated arm and says so.
type RWACuratedDirectoryReader interface {
	CuratedRWADirectoryByAddress(ctx context.Context, curator string) (
		map[string]timescale.CuratedRWAEntry, timescale.CuratedRWACensus, error)
}

// rwaCuratedSnapshotTTL bounds how often the cache is re-read on the hot
// path. The sync behind it is daily; ten minutes matches the membership
// rebuild it rides alongside.
const rwaCuratedSnapshotTTL = 10 * time.Minute

type rwaCurated struct {
	byAddress  map[string]timescale.CuratedRWAEntry
	census     timescale.CuratedRWACensus
	available  bool
	wired      bool
	observedAt time.Time
}

type rwaCuratedCache struct {
	mu   sync.Mutex
	snap rwaCurated
	at   time.Time
}

// rwaCuratedSnapshot fails CLOSED like every other snapshot on this
// surface: a failed read yields an unavailable snapshot and is cached as
// such, never the last good one.
func (s *Server) rwaCuratedSnapshot(ctx context.Context) rwaCurated {
	s.rwaCuratedSnap.mu.Lock()
	defer s.rwaCuratedSnap.mu.Unlock()
	if !s.rwaCuratedSnap.at.IsZero() && time.Since(s.rwaCuratedSnap.at) < rwaCuratedSnapshotTTL {
		return s.rwaCuratedSnap.snap
	}
	s.rwaCuratedSnap.at = time.Now()
	s.rwaCuratedSnap.snap = s.readRWACurated(ctx)
	return s.rwaCuratedSnap.snap
}

func (s *Server) readRWACurated(ctx context.Context) rwaCurated {
	if s.rwaCurated == nil {
		return rwaCurated{}
	}
	rows, census, err := s.rwaCurated.CuratedRWADirectoryByAddress(ctx, rwaCuratorDune)
	if err != nil {
		s.logger.Warn("rwa curated directory read failed", "err", err)
		return rwaCurated{wired: true, observedAt: time.Now()}
	}
	if len(rows) == 0 {
		s.logger.Warn("rwa curated directory: no fresh rows", "stale", census.Stale)
		return rwaCurated{wired: true, census: census, observedAt: time.Now()}
	}
	return rwaCurated{byAddress: rows, census: census, available: true, wired: true, observedAt: time.Now()}
}

// ─── wire shapes ────────────────────────────────────────────────────

// RWACurator is what the curator said about a row, served verbatim.
type RWACurator struct {
	Curator string `json:"curator"`
	// Company and Subclass are the curator's labels, not this index's
	// vocabulary. A subclass of "US Treasuries" is not `anchor_class:
	// bond`, and the two are never mapped onto each other.
	Company  string `json:"company,omitempty"`
	Subclass string `json:"subclass,omitempty"`
	// AlsoVerified is true when the SAME address is in the verified set
	// above; such a row is served here for reconciliation and its value
	// is NOT added to the curated total, or it would be counted twice
	// in `combined_value_usd`.
	AlsoVerified bool `json:"also_verified"`
}

// RWACuratedCensus is the storage layer's account of the curator cache.
type RWACuratedCensus struct {
	Entries    int       `json:"entries"`
	Priced     int       `json:"priced"`
	Stale      int       `json:"stale"`
	ObservedAt *WireTime `json:"observed_at,omitempty"`
}

// RWACuratedSummary is the curated arm's own total. It sits BESIDE the
// verified summary and is never folded into it.
type RWACuratedSummary struct {
	Curator string `json:"curator"`
	// Status is "served", "unavailable" (the reader failed or the cache
	// is entirely stale) or "unwired" (no reader configured).
	Status string `json:"status"`
	// Assets counts curated rows served; AlsoVerified counts the subset
	// the verified set already carries.
	Assets       int `json:"assets"`
	AlsoVerified int `json:"also_verified"`
	AssetsValued int `json:"assets_valued"`
	// AdditionalValueUSD sums the curator-priced valuations of rows the
	// verified set does NOT carry — what the curator counts that this
	// index cannot verify.
	AdditionalValueUSD *string `json:"additional_value_usd,omitempty"`
	// CombinedValueUSD is the verified reference total plus
	// AdditionalValueUSD: the figure a reader gets by counting the way
	// the curator counts. Published so the comparison is one number on
	// one page, and labelled so it cannot be mistaken for the verified
	// total beside it.
	CombinedValueUSD *string `json:"combined_value_usd,omitempty"`
	// VerifiedValueUSD repeats summary.reference_valuation.value_usd so
	// the three figures read together.
	VerifiedValueUSD *string          `json:"verified_value_usd,omitempty"`
	Census           RWACuratedCensus `json:"census"`
	Basis            string           `json:"basis"`
}

const rwaCuratedBasisProse = "Rows a named third-party curator lists as tokenized real-world assets on Stellar, valued at that curator's own " +
	"published price per token times the supply this index reads from the lake. Nothing here is verified by this index: no " +
	"issuer declaration, no directory attestation, no oracle, no market. The curator's company and subclass labels are served " +
	"verbatim and never mapped onto this index's vocabulary. `additional_value_usd` sums only rows the verified set does not " +
	"carry; `combined_value_usd` adds that to the verified reference total and is the figure a reader gets by counting the way " +
	"the curator counts. It is published so the comparison is one number on one page — not because this index vouches for it."

// ─── membership ─────────────────────────────────────────────────────

// rwaCuratedMember is one curated row before it is served.
type rwaCuratedMember struct {
	contractID   string
	entry        timescale.CuratedRWAEntry
	alsoVerified bool
}

// rwaCuratedMembership selects the curator's rows and marks which of them
// the verified set already carries. A classic verified member is matched
// by the Stellar Asset Contract its (code, issuer) derives — a pure
// function of the pair and the network, so an impersonator's SAC never
// collides with the genuine one.
func rwaCuratedMembership(snap rwaCurated, verified []RWAAsset) []rwaCuratedMember {
	if !snap.available {
		return nil
	}
	seen := make(map[string]struct{}, len(verified))
	for _, a := range verified {
		if a.ContractID != "" {
			seen[a.ContractID] = struct{}{}
			continue
		}
		if a.Code != "" && a.Issuer != "" {
			if sac, err := (canonical.Asset{Code: a.Code, Issuer: a.Issuer}).SacContractID(); err == nil {
				seen[sac] = struct{}{}
			}
		}
	}
	out := make([]rwaCuratedMember, 0, len(snap.byAddress))
	for addr, e := range snap.byAddress {
		if !rwaCuratedAddressIsContract(addr) {
			continue
		}
		_, also := seen[addr]
		out = append(out, rwaCuratedMember{contractID: addr, entry: e, alsoVerified: also})
	}
	sortCuratedMembers(out)
	return out
}

func rwaCuratedAddressIsContract(addr string) bool {
	return len(addr) == 56 && addr[0] == 'C'
}

func sortCuratedMembers(m []rwaCuratedMember) {
	for i := 1; i < len(m); i++ {
		for j := i; j > 0 && m[j].contractID < m[j-1].contractID; j-- {
			m[j], m[j-1] = m[j-1], m[j]
		}
	}
}

// ─── rows ───────────────────────────────────────────────────────────

// rwaCuratedRows turns members into served rows through the SAME
// catalogue, supply and decimals path the contract arm uses, so a
// curated row's supply is the lake's reading and not the curator's.
func (s *Server) rwaCuratedRows(ctx context.Context, members []rwaCuratedMember, now time.Time) ([]RWAAsset, int) {
	if len(members) == 0 {
		return nil, 0
	}
	cm := make([]rwaContractMember, 0, len(members))
	for _, m := range members {
		cm = append(cm, rwaContractMember{
			contractID:  m.contractID,
			symbol:      m.entry.AssetCode,
			basis:       rwa.BasisThirdPartyCurated,
			recognition: rwa.RecognitionThirdPartyCurator,
			dirName:     m.entry.Company,
		})
	}
	details, notObserved, err := s.rwaContractListingRows(ctx, cm)
	if err != nil {
		s.logger.Error("rwa curated listing read failed", "err", err)
		return nil, len(members)
	}
	rows := rwaContractAssetRows(cm, details)
	byID := make(map[string]rwaCuratedMember, len(members))
	for _, m := range members {
		byID[m.contractID] = m
	}
	for i := range rows {
		m := byID[rows[i].ContractID]
		rows[i].Curator = &RWACurator{
			Curator:      rwaCuratorDune,
			Company:      m.entry.Company,
			Subclass:     m.entry.AssetSubclass,
			AlsoVerified: m.alsoVerified,
		}
		rwaApplyCuratorReference(&rows[i], m.entry, now)
	}
	rwaSortAssets(rows)
	return rows, notObserved
}

// rwaApplyCuratorReference attaches the curator's price as the row's
// reference. The provenance says it is an uploaded price; no premium is
// published against it, for a stronger version of the reason none is
// published against a listing price.
func rwaApplyCuratorReference(a *RWAAsset, e timescale.CuratedRWAEntry, now time.Time) {
	if a.Valuation.Status == RWAValuationIssuerFlagged {
		rwaRefuseReference(a, RWAPremiumIssuerFlagged)
		return
	}
	entry := timescale.ListingEntry{
		Address: e.Address, ListingID: e.Company, Symbol: e.AssetCode,
		PriceUSD: e.PriceUSD, PricedAt: e.PricedAt, Source: e.Source,
	}
	rwaApplyListingReference(a, entry, RWAPremiumContractNotBound, now)
	if a.Reference != nil {
		a.Reference.Provenance = RWAReferenceCuratorPrice
		a.Reference.Feed = rwaCuratorDune
	}
}

// ─── summary ────────────────────────────────────────────────────────

func rwaCuratedSummarise(snap rwaCurated, rows []RWAAsset, verifiedRef *string) *RWACuratedSummary {
	out := &RWACuratedSummary{
		Curator: rwaCuratorDune,
		Basis:   rwaCuratedBasisProse,
		Census: RWACuratedCensus{
			Entries: snap.census.Entries, Priced: snap.census.Priced, Stale: snap.census.Stale,
		},
		VerifiedValueUSD: verifiedRef,
	}
	if !snap.observedAt.IsZero() {
		t := WireTime(snap.observedAt)
		out.Census.ObservedAt = &t
	}
	switch {
	case !snap.wired:
		out.Status = "unwired"
		return out
	case !snap.available:
		out.Status = "unavailable"
		return out
	}
	out.Status = "served"
	out.Assets = len(rows)
	additional := new(big.Rat)
	anyAdditional := false
	for _, r := range rows {
		if r.Curator != nil && r.Curator.AlsoVerified {
			out.AlsoVerified++
		}
		if r.ReferenceValuation.ValueUSD == nil {
			continue
		}
		out.AssetsValued++
		if r.Curator != nil && r.Curator.AlsoVerified {
			continue
		}
		if v := ratFromOptionalString(r.ReferenceValuation.ValueUSD); v != nil {
			additional.Add(additional, v)
			anyAdditional = true
		}
	}
	if anyAdditional {
		s := additional.FloatString(2)
		out.AdditionalValueUSD = &s
		if verifiedRef != nil {
			if vr := ratFromOptionalString(verifiedRef); vr != nil {
				c := new(big.Rat).Add(vr, additional).FloatString(2)
				out.CombinedValueUSD = &c
			}
		}
	}
	return out
}

// ─── handler hook ───────────────────────────────────────────────────

// attachRWACurated runs the curated arm after the verified view is
// complete and attaches its rows and total to the view, apart.
func (s *Server) attachRWACurated(r *http.Request, view *RWAAssetsView, now time.Time) {
	snap := s.rwaCuratedSnapshot(r.Context())
	members := rwaCuratedMembership(snap, view.Assets)
	rows, _ := s.rwaCuratedRows(r.Context(), members, now)
	view.CuratedAssets = rows
	view.Curated = rwaCuratedSummarise(snap, rows, view.Summary.ReferenceValuation.ValueUSD)
}
