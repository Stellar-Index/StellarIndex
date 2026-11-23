package v1

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// Look-backs of the values an asset row serves: the snapshot price's maximum age, and each change's horizon plus the slack its
// reference bucket may sit before it (see timescale.AssetRow).
const (
	assetPriceExclusionSpan = 15 * time.Minute
	assetVolumeSpan         = 24 * time.Hour
	assetSurfaceSpan        = 7*24*time.Hour + 2*time.Hour
	// assetATHSpan brackets an ATH's instant: its bucket grain is not recorded, so the day either side is judged.
	assetATHSpan = 24 * time.Hour
)

// assetExclusionScope is one read of the exclusion set judging an asset listing page or detail on the way out. The in-process
// caches hold unjudged values, so they are read and filled whatever the set holds and only the way out pays for a judgement.
type assetExclusionScope struct {
	set    pricingguard.ExclusionSet
	judge  bool // a non-empty, readable set
	unread bool
	now    time.Time
}

func (s *Server) assetExclusionScope(ctx context.Context) assetExclusionScope {
	sc := assetExclusionScope{now: time.Now()}
	set, judge, v := s.exclusionSnapshot(ctx)
	switch {
	case v != pricingguard.NotWithheld:
		sc.unread = true
	case judge:
		sc.set, sc.judge = set, true
	}
	return sc
}

// empty: the set withholds nothing, so a cached body is written byte for byte.
func (sc assetExclusionScope) empty() bool { return !sc.judge && !sc.unread }

// judgeRows withholds what an exclusion covers on each row and puts it on flags: an unread set withholds every price-derived
// value (stale, as a transient outage); a removal marks fills_withheld_reason. Either is degraded, so no-store.
func (sc assetExclusionScope) judgeRows(ctx context.Context, s *Server, rows []AssetDetail, flags Flags) Flags {
	removed := false
	for i := range rows {
		// Judging writes into these in place; the row may share them with a cached entry.
		rows[i].PriceHistory24h, rows[i].PriceHistory7d = slices.Clone(rows[i].PriceHistory24h), slices.Clone(rows[i].PriceHistory7d)
		rows[i].TopMarkets = slices.Clone(rows[i].TopMarkets)
		if sc.unread {
			removed = withholdAssetRow(&rows[i], withheldReasonFor(pricingguard.WithheldExclusionsUnread)) || removed
		} else if sc.judge {
			removed = sc.judgeRow(ctx, s, &rows[i]) || removed
		}
	}
	if sc.unread && removed {
		flags.Stale, flags.Degraded = true, true
		return flags
	}
	return markFillsWithheld(flags, removed)
}

// judgeRow applies the unattributed rule to one row, except a headline that records its sources. The listing price may be
// read through XLM (the snapshot's XLM arm), so an exclusion naming XLM covers it too, as for the global headline.
func (sc assetExclusionScope) judgeRow(ctx context.Context, s *Server, d *AssetDetail) bool {
	asset, known := assetRowIdentity(s, d)
	covered := sc.rowCovered(ctx, s, d, asset, known)
	now := sc.now.Add(time.Nanosecond)
	removed := false
	if d.PriceUSD != nil && sc.headlineCovered(ctx, d, asset, known, covered, now) {
		removed = withholdAssetRow(d, PriceWithheldSourceExcluded)
	}
	for _, c := range []struct {
		v    **string
		back time.Duration
	}{{&d.Change1hPct, time.Hour + 5*time.Minute}, {&d.Change24hPct, 24*time.Hour + 30*time.Minute}, {&d.Change7dPct, assetSurfaceSpan}} {
		if *c.v != nil && covered(now.Add(-c.back), now) {
			*c.v, removed = nil, true
		}
	}
	if covered(now.Add(-assetVolumeSpan), now) {
		removed = withholdAssetVolume(d) || removed
	}
	d.PriceHistory24h, removed = sc.cleanSeries(d.PriceHistory24h, covered, removed)
	d.PriceHistory7d, removed = sc.cleanSeries(d.PriceHistory7d, covered, removed)
	if d.ATH != nil {
		at, err := time.Parse(time.RFC3339Nano, d.ATH.At)
		if err != nil || covered(at.Add(-assetATHSpan), at.Add(assetATHSpan)) {
			d.ATH, removed = nil, true
		}
	}
	return removed
}

// rowCovered judges a span of a row's unattributed values: on the asset and every leg it may be priced through, or, for an
// asset that does not resolve, by any exclusion in force.
func (sc assetExclusionScope) rowCovered(
	ctx context.Context, s *Server, d *AssetDetail, asset canonical.Asset, known bool,
) func(from, to time.Time) bool {
	legs := []canonical.Asset{asset, canonical.NativeAsset()}
	// A declared non-USD peg is filled through its fiat's FX rate.
	if peg, ok := s.FiatPeggedClassics[d.AssetID]; ok && !peg.Equal(defaultPriceQuote) {
		legs = append(legs, peg)
	}
	return func(from, to time.Time) bool {
		if !from.Before(to) {
			to = from.Add(time.Nanosecond)
		}
		if !known {
			return sc.set.MultiHopWithholding(ctx, from, to) != pricingguard.NotWithheld
		}
		for _, a := range legs {
			if sc.set.UnattributedWithholding(ctx, a, defaultPriceQuote, from, to) != pricingguard.NotWithheld {
				return true
			}
		}
		return false
	}
}

// headlineCovered judges price_usd over the snapshot's age: on its recorded sources when it has them, else unattributed.
func (sc assetExclusionScope) headlineCovered(
	ctx context.Context, d *AssetDetail, asset canonical.Asset, known bool, covered func(from, to time.Time) bool, now time.Time,
) bool {
	from := now.Add(-assetPriceExclusionSpan)
	if !known || len(d.PriceSources) == 0 {
		return covered(from, now)
	}
	return judgeBucket(ctx, sc.set, []canonical.Pair{{Base: asset, Quote: defaultPriceQuote}}, d.PriceSources, from, now) != pricingguard.NotWithheld
}

// cleanSeries nulls each point whose span (from the previous point) an exclusion covers: a gap where it was, never a zero.
func (sc assetExclusionScope) cleanSeries(pts []AssetPricePoint, covered func(from, to time.Time) bool, removed bool) ([]AssetPricePoint, bool) {
	var prev time.Time
	for i := range pts {
		t, err := time.Parse(time.RFC3339Nano, pts[i].T)
		from := prev
		if prev.IsZero() {
			from = t
		}
		if pts[i].P != nil && (err != nil || covered(from, t)) {
			pts[i].P, removed = nil, true
		}
		prev = t
	}
	return pts, removed
}

// assetRowIdentity is the asset a row's prices are read for; a catalogue row is its Stellar twin. known=false: none resolves.
func assetRowIdentity(s *Server, d *AssetDetail) (canonical.Asset, bool) {
	id := d.AssetID
	if d.Type == assetTypeGlobal {
		id = s.seriesAssetIDForRow(d)
	}
	a, err := canonical.ParseAsset(id)
	return a, err == nil && id != ""
}

// withholdAssetRow withholds price_usd and every value derived from it for reason; false when the row served none.
func withholdAssetRow(d *AssetDetail, reason PriceWithheldReason) bool {
	had := d.PriceUSD != nil || d.Change1hPct != nil || d.Change24hPct != nil || d.Change7dPct != nil || d.MarketCapUSD != nil ||
		d.FDVUSD != nil || d.ATH != nil || d.PriceHistory24h != nil || d.PriceHistory7d != nil || d.ListingValuation != nil ||
		d.ListingReference != nil || d.GlobalMarket != nil
	if d.PriceUSD != nil {
		d.PriceWithheldReason = reason
	}
	unpublishPrice(d)
	d.ListingReference, d.ListingValuation, d.GlobalMarket = nil, nil, nil
	if reason != PriceWithheldSourceExcluded {
		had = withholdAssetVolume(d) || had
	}
	return had
}

// withholdAssetVolume drops the 24h USD volume, the row's and each top market's; false when none was served.
func withholdAssetVolume(d *AssetDetail) bool {
	had := d.VolumeUSD24h != nil
	d.VolumeUSD24h, d.VolumeLowerBound = nil, false
	for i := range d.TopMarkets {
		had = had || d.TopMarkets[i].Volume24hUSD != nil
		d.TopMarkets[i].Volume24hUSD = nil
	}
	return had
}

// judgeDetail is [assetExclusionScope.judgeRows] for one detail payload.
func (sc assetExclusionScope) judgeDetail(ctx context.Context, s *Server, d *AssetDetail, flags Flags) Flags {
	if sc.empty() {
		return flags
	}
	rows := []AssetDetail{*d}
	flags = sc.judgeRows(ctx, s, rows, flags)
	*d = rows[0]
	return flags
}

// writeDetail writes a detail entry judged on the way out; the entry, cached or not, stays unjudged.
func (sc assetExclusionScope) writeDetail(ctx context.Context, w http.ResponseWriter, s *Server, e *assetDetailEntry) {
	if sc.empty() {
		writeCachedAssetDetail(w, e)
		return
	}
	d := e.detail
	flags := sc.judgeDetail(ctx, s, &d, e.flags)
	body, err := renderAssetDetailEnvelope(d, flags)
	if err != nil {
		writeJSON(w, d, flags)
		return
	}
	writeCachedAssetDetail(w, newAssetDetailEntry(d, body, flags))
}

// replayAssetListing writes a cached listing page: its bytes under an empty set, else its unjudged envelope judged.
func (s *Server) replayAssetListing(w http.ResponseWriter, r *http.Request, sc assetExclusionScope, hit assetListHit) {
	if sc.empty() {
		writeCachedAssetList(w, hit.body, hit.stale)
		return
	}
	w.Header().Set("X-Stellarindex-Cache", "HIT")
	s.writeAssetListing(w, r, sc, hit.env)
}

// writeAssetListing judges a copy of a listing envelope's rows on the way out, after any cache, then writes it; the rows may be
// the ones a cache holds.
func (s *Server) writeAssetListing(w http.ResponseWriter, r *http.Request, sc assetExclusionScope, env Envelope) {
	if rows, ok := env.Data.([]AssetDetail); ok && !sc.empty() {
		rows = slices.Clone(rows)
		env.Flags, env.Data = sc.judgeRows(r.Context(), s, rows, env.Flags), rows
	}
	writeEnvelope(w, env)
}
