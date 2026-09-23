// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"math/big"
	"net/http"
	"strconv"
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
//
// Two reads, one seam. The per-asset directory is what the arm was
// built on; the first curator's per-asset list and prices turned out to
// be PRIVATE uploads (see internal/ops/ingest/curated_rwa_sync.go), so
// in practice it is the curator's PUBLISHED totals that answer, and the
// directory stays empty until a curator whose list is readable exists.
type RWACuratedDirectoryReader interface {
	CuratedRWADirectoryByAddress(ctx context.Context, curator string) (
		map[string]timescale.CuratedRWAEntry, timescale.CuratedRWACensus, error)
	// LatestCuratedPublished returns what the curator last published —
	// its latest monthly total, that month's split and the full series
	// — or (nil, nil) when nothing is inside the recognition bound.
	LatestCuratedPublished(ctx context.Context, curator string) (*timescale.CuratedRWAPublished, error)
}

// rwaCuratedSnapshotTTL bounds how often the cache is re-read on the hot
// path. The sync behind it is daily; ten minutes matches the membership
// rebuild it rides alongside.
const rwaCuratedSnapshotTTL = 10 * time.Minute

// rwaCuratedReadBudget bounds one refresh read. The read is detached
// from the triggering request and runs under the cache mutex every /rwa
// request queues on, so it needs a deadline of its own.
const rwaCuratedReadBudget = 5 * time.Second

type rwaCurated struct {
	byAddress map[string]timescale.CuratedRWAEntry
	census    timescale.CuratedRWACensus
	// published is what the curator last published, when the read
	// answered and something is inside its bound; nil otherwise.
	published *timescale.CuratedRWAPublished
	// available says the per-asset directory answered with rows.
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
//
// The read runs on a context DETACHED from the caller's request, so the
// request that triggers a refresh cannot poison the shared cache by
// disconnecting mid-read, and under its own deadline, so a hung read
// cannot hold the mutex indefinitely. The TTL is stamped only once the
// read has returned.
func (s *Server) rwaCuratedSnapshot(ctx context.Context) rwaCurated {
	return s.rwaCuratedSnapshotWithin(ctx, rwaCuratedReadBudget)
}

func (s *Server) rwaCuratedSnapshotWithin(ctx context.Context, budget time.Duration) rwaCurated {
	s.rwaCuratedSnap.mu.Lock()
	defer s.rwaCuratedSnap.mu.Unlock()
	if !s.rwaCuratedSnap.at.IsZero() && time.Since(s.rwaCuratedSnap.at) < rwaCuratedSnapshotTTL {
		return s.rwaCuratedSnap.snap
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	defer cancel()
	snap := s.readRWACurated(rctx)
	s.rwaCuratedSnap.snap = snap
	s.rwaCuratedSnap.at = time.Now()
	return snap
}

func (s *Server) readRWACurated(ctx context.Context) rwaCurated {
	if s.rwaCurated == nil {
		return rwaCurated{}
	}
	// The two reads fail independently: a curator whose published totals
	// answer while its per-asset list is (as today) unreadable is the
	// normal state, not a failure of either.
	published, err := s.rwaCurated.LatestCuratedPublished(ctx, rwaCuratorDune)
	if err != nil {
		s.logger.Warn("rwa curated published read failed", "err", err)
		published = nil
	}
	rows, census, err := s.rwaCurated.CuratedRWADirectoryByAddress(ctx, rwaCuratorDune)
	if err != nil {
		s.logger.Warn("rwa curated directory read failed", "err", err)
		return rwaCurated{published: published, wired: true, observedAt: time.Now()}
	}
	if len(rows) == 0 {
		if published == nil {
			s.logger.Warn("rwa curated directory: no fresh rows and nothing published", "stale", census.Stale)
		}
		return rwaCurated{published: published, wired: true, census: census, observedAt: time.Now()}
	}
	return rwaCurated{byAddress: rows, census: census, published: published, available: true, wired: true, observedAt: time.Now()}
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

// RWACuratedPublishedSplit is one line of the curator's own subclass
// split for the published month, labelled in the curator's vocabulary.
type RWACuratedPublishedSplit struct {
	Subclass string `json:"subclass"`
	ValueUSD string `json:"value_usd"`
}

// RWACuratedPublishedPoint is one month of the curator's published
// total series.
type RWACuratedPublishedPoint struct {
	// MonthEnd is the last day of the month, YYYY-MM-DD.
	MonthEnd string `json:"month_end"`
	ValueUSD string `json:"value_usd"`
}

// rwaCuratedPublishedStaleAfter is the executed_at age past which a
// published total is labelled `stale`. executed_at is the curator's
// clock; observed_at only proves our own sync ran. 48h mirrors the
// storage layer's curatedRWARecognitionMaxAge.
const rwaCuratedPublishedStaleAfter = 48 * time.Hour

// rwaCuratedPublishedMaxAge is the executed_at age past which a
// published total is not served at all; it matches the sibling
// directory table's curatedRWAPriceMaxAge.
const rwaCuratedPublishedMaxAge = 7 * 24 * time.Hour

// RWACuratedPublished is what the curator PUBLISHES: the headline
// monthly RWA market-cap total its public dashboard queries last
// computed, that month's split by the curator's own subclass labels,
// and the full monthly series. The curator's arithmetic over the
// curator's private inputs — no per-asset breakdown reaches this index
// — served verbatim so the gap to the verified total is one number.
type RWACuratedPublished struct {
	// TotalUSD is the latest month's total, 2dp.
	TotalUSD string `json:"total_usd"`
	// AsOf is the month the total is for: its last day, YYYY-MM-DD.
	AsOf string `json:"as_of"`
	// ExecutedAt is when the curator's query last ran — the figure's
	// own freshness, distinct from when this index read it.
	ExecutedAt WireTime                   `json:"executed_at"`
	BySubclass []RWACuratedPublishedSplit `json:"by_subclass"`
	Series     []RWACuratedPublishedPoint `json:"series"`
	// Stale marks a published total whose ExecutedAt is older than
	// [rwaCuratedPublishedStaleAfter]: the curator's own query has not
	// moved in a while, even though this index's read of it is fresh.
	Stale bool `json:"stale,omitempty"`
	// Source names the curator's public queries the figures were read
	// from, e.g. "dune query 6961845 / 6961847".
	Source string `json:"source"`
	// GapVsVerifiedUSD is TotalUSD minus this index's verified reference
	// total, signed, 2dp: what the curator counts that this index does
	// not verify (or, negative, the reverse). Absent when the verified
	// set publishes no reference total to compare against.
	GapVsVerifiedUSD *string `json:"gap_vs_verified_usd,omitempty"`
}

// RWACuratedSummary is the curated arm's own total. It sits BESIDE the
// verified summary and is never folded into it.
type RWACuratedSummary struct {
	Curator string `json:"curator"`
	// Status is "served" (the curator's per-asset rows are below),
	// "published_totals" (no per-asset row is readable, and the
	// curator's published totals are in Published), "unavailable"
	// (neither answered inside its bound) or "unwired" (no reader
	// configured).
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
	// Published is what the curator publishes about its own list —
	// the latest monthly total, its subclass split and the series —
	// read from the curator's public queries because the list itself
	// is private. Nil when nothing published is inside its bound.
	Published *RWACuratedPublished `json:"published,omitempty"`
	Basis     string               `json:"basis"`
}

const rwaCuratedBasisProse = "What a named third-party curator counts as tokenized real-world assets on Stellar, beside what this index " +
	"verifies. The curator's per-asset list and its per-asset prices are PRIVATE uploads on the curator's platform, refused to " +
	"every outside account, so this index cannot read them and admits no row on them; only the totals the curator PUBLISHES " +
	"from them are read. `published` carries the curator's latest monthly RWA market-cap total, that month's split by the " +
	"curator's own subclass labels and the full monthly series, exactly as the curator's public dashboard queries last " +
	"computed them, and `gap_vs_verified_usd` is that total minus this index's verified reference total, signed. Nothing here " +
	"is verified by this index: no issuer declaration, no directory attestation, no oracle, no market, and no per-asset " +
	"breakdown reaches it. Should a curator's per-asset list become readable, its rows are served in `curated_assets` at that " +
	"curator's own price times the supply this index reads from the lake, with the curator's labels verbatim and never mapped " +
	"onto this index's vocabulary; `additional_value_usd` then sums only rows the verified set does not carry and " +
	"`combined_value_usd` adds that to the verified reference total. All of it is published so the comparison is one number " +
	"on one page — not because this index vouches for any of it."

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

func rwaCuratedSummarise(snap rwaCurated, rows []RWAAsset, verifiedRef *string, now time.Time) *RWACuratedSummary {
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
	if snap.wired {
		out.Published = rwaCuratedPublishedBlock(snap.published, verifiedRef, now)
	}
	switch {
	case !snap.wired:
		out.Status = "unwired"
		return out
	case !snap.available && out.Published != nil:
		out.Status = "published_totals"
		return out
	case !snap.available:
		out.Status = "unavailable"
		return out
	}
	out.Status = "served"
	out.Assets = len(rows)
	additional, anyAdditional := rwaCuratedTally(rows, out)
	if anyAdditional {
		s := additional.FloatString(2)
		out.AdditionalValueUSD = &s
		if vr := ratFromOptionalString(verifiedRef); vr != nil {
			c := new(big.Rat).Add(vr, additional).FloatString(2)
			out.CombinedValueUSD = &c
		}
	}
	return out
}

// rwaCuratedTally counts the served rows into the summary — how many the
// verified set also carries, how many carry a value — and sums the value
// of the rows the verified set does NOT carry, which is the only part
// that adds to the verified total. The bool says whether any such row
// carried a value at all, so an all-verified set serves no "additional"
// figure rather than a zero.
func rwaCuratedTally(rows []RWAAsset, out *RWACuratedSummary) (*big.Rat, bool) {
	additional := new(big.Rat)
	anyAdditional := false
	for _, r := range rows {
		alsoVerified := r.Curator != nil && r.Curator.AlsoVerified
		if alsoVerified {
			out.AlsoVerified++
		}
		if r.ReferenceValuation.ValueUSD == nil {
			continue
		}
		out.AssetsValued++
		if alsoVerified {
			continue
		}
		if v := ratFromOptionalString(r.ReferenceValuation.ValueUSD); v != nil {
			additional.Add(additional, v)
			anyAdditional = true
		}
	}
	return additional, anyAdditional
}

// rwaCuratedPublishedBlock renders what the curator last published, with
// the signed gap to the verified reference total. Every figure is the
// stored decimal re-rendered at 2dp through big.Rat, never a float; the
// series and the split are served in the order the reader returned them
// (oldest month first; largest subclass first).
//
// A total whose ExecutedAt is past [rwaCuratedPublishedMaxAge] is not
// served at all — the curator's own clock, not this index's sync
// health, has gone stale for too long. Between that and
// [rwaCuratedPublishedStaleAfter] it is served labelled `stale: true`.
func rwaCuratedPublishedBlock(p *timescale.CuratedRWAPublished, verifiedRef *string, now time.Time) *RWACuratedPublished {
	if p == nil {
		return nil
	}
	if p.ExecutedAt.IsZero() || now.Sub(p.ExecutedAt) > rwaCuratedPublishedMaxAge {
		return nil
	}
	total := ratFromOptionalString(&p.TotalUSD)
	if total == nil {
		return nil
	}
	out := &RWACuratedPublished{
		TotalUSD:   total.FloatString(2),
		AsOf:       p.MonthEnd.UTC().Format("2006-01-02"),
		ExecutedAt: WireTime(p.ExecutedAt),
		BySubclass: make([]RWACuratedPublishedSplit, 0, len(p.BySubclass)),
		Series:     make([]RWACuratedPublishedPoint, 0, len(p.Series)),
		Stale:      now.Sub(p.ExecutedAt) > rwaCuratedPublishedStaleAfter,
		Source:     "dune query " + strconv.FormatInt(p.SourceQuery, 10),
	}
	if p.SplitSourceQuery != 0 && p.SplitSourceQuery != p.SourceQuery {
		out.Source += " / " + strconv.FormatInt(p.SplitSourceQuery, 10)
	}
	for _, sp := range p.BySubclass {
		if v := ratFromOptionalString(&sp.ValueUSD); v != nil {
			out.BySubclass = append(out.BySubclass, RWACuratedPublishedSplit{Subclass: sp.Subclass, ValueUSD: v.FloatString(2)})
		}
	}
	for _, pt := range p.Series {
		if v := ratFromOptionalString(&pt.ValueUSD); v != nil {
			out.Series = append(out.Series, RWACuratedPublishedPoint{MonthEnd: pt.MonthEnd.UTC().Format("2006-01-02"), ValueUSD: v.FloatString(2)})
		}
	}
	if vr := ratFromOptionalString(verifiedRef); vr != nil {
		gap := new(big.Rat).Sub(total, vr).FloatString(2)
		out.GapVsVerifiedUSD = &gap
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
	view.Curated = rwaCuratedSummarise(snap, rows, view.Summary.ReferenceValuation.ValueUSD, now)
}
