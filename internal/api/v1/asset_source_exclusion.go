package v1

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// Look-backs of the values an asset row serves (see timescale's asset_price_snapshot). The snapshot row does not record the
// minute its price was read at, only that it is the newest traded minute in the 7 days before the row was computed, so the
// headline is judged over that whole lookback.
const (
	assetSnapshotMaxAge = 15 * time.Minute
	assetPriceLookback  = 7*24*time.Hour + assetSnapshotMaxAge
	// assetAnchorMaxAge: an XLM-routed price multiplies by the XLM/USD anchor up to an hour before its own minute.
	assetAnchorMaxAge = time.Hour
	assetVolumeSpan   = 24*time.Hour + assetSnapshotMaxAge
	assetSurfaceSpan  = 7*24*time.Hour + 2*time.Hour + assetSnapshotMaxAge
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
	j := sc.rowJudge(ctx, s, d)
	covered := j.covered
	now := sc.now.Add(time.Nanosecond)
	removed := false
	if d.PriceUSD != nil && j.headlineCovered(d, now) {
		removed = withholdAssetRow(d, PriceWithheldSourceExcluded)
	}
	for _, c := range []struct {
		v    **string
		back time.Duration
	}{
		{&d.Change1hPct, time.Hour + 5*time.Minute + assetSnapshotMaxAge},
		{&d.Change24hPct, 24*time.Hour + 30*time.Minute + assetSnapshotMaxAge},
		{&d.Change7dPct, assetSurfaceSpan},
	} {
		if *c.v != nil && covered(now.Add(-c.back), now) {
			*c.v, removed = nil, true
		}
	}
	if covered(now.Add(-assetVolumeSpan), now) {
		removed = withholdAssetVolume(d) || removed
	}
	d.PriceHistory24h, removed = cleanSeries(d.PriceHistory24h, covered, now, removed)
	d.PriceHistory7d, removed = cleanSeries(d.PriceHistory7d, covered, now, removed)
	if d.ATH != nil {
		at, err := time.Parse(time.RFC3339Nano, d.ATH.At)
		if err != nil || covered(at.Add(-assetATHSpan), at.Add(assetATHSpan)) {
			d.ATH, removed = nil, true
		}
	}
	return removed
}

// xlmAnchorQuotes are the USD proxies the XLM/USD anchor is read against (timescale's usdProxyQuotes); XLM's alias forms are
// matched by the exclusion's own family check.
var xlmAnchorQuotes = []canonical.Asset{
	mustParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"),
	mustParseAsset("CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"),
	defaultPriceQuote,
}

// rowJudge judges one row's unattributed values on every leg it may be priced through: the asset's own books, XLM's (an
// XLM-routed price) and a declared non-USD peg's FX; an asset that does not resolve is judged by any exclusion in force.
type rowJudge struct {
	ctx   context.Context
	set   pricingguard.ExclusionSet
	asset canonical.Asset
	known bool
	xlm   bool // the row is XLM itself, so its own leg is the XLM/USD anchor
	peg   *canonical.Asset
}

func (sc assetExclusionScope) rowJudge(ctx context.Context, s *Server, d *AssetDetail) rowJudge {
	j := rowJudge{ctx: ctx, set: sc.set}
	j.asset, j.known = assetRowIdentity(s, d)
	j.xlm = j.known && slices.Contains(canonical.AssetAliasStrings(canonical.NativeAsset()), j.asset.String())
	if peg, ok := s.FiatPeggedClassics[d.AssetID]; ok && !peg.Equal(defaultPriceQuote) {
		j.peg = &peg
	}
	return j
}

// covered judges every leg over one span.
func (j rowJudge) covered(from, to time.Time) bool { return j.legsCovered(from, from, from, to) }

// legsCovered judges the asset's leg from own, the XLM leg from xlm and the peg's FX from fx, each to to.
func (j rowJudge) legsCovered(own, xlm, fx, to time.Time) bool {
	if !own.Before(to) {
		to = own.Add(time.Nanosecond)
	}
	if !j.known {
		from := own
		for _, t := range []time.Time{xlm, fx} {
			if t.Before(from) {
				from = t
			}
		}
		return j.set.MultiHopWithholding(j.ctx, from, to) != pricingguard.NotWithheld
	}
	if !j.xlm && j.set.UnattributedWithholding(j.ctx, j.asset, defaultPriceQuote, own, to) != pricingguard.NotWithheld {
		return true
	}
	for _, q := range xlmAnchorQuotes {
		// The anchor reads XLM's own USD books only, so an exclusion on another XLM market (AQUA/XLM) does not reach it.
		if j.set.PairWithholding(j.ctx, canonical.NativeAsset(), q, xlm, to) != pricingguard.NotWithheld {
			return true
		}
	}
	return j.peg != nil && j.set.UnattributedWithholding(j.ctx, *j.peg, defaultPriceQuote, fx, to) != pricingguard.NotWithheld
}

// headlineCovered judges price_usd over the lookback its minute may sit in, as the row does not carry the minute: the
// newest traded minute in [assetPriceLookback]; for XLM itself the anchor at the row's computation; for an XLM-routed price the
// anchor up to [assetAnchorMaxAge] before that minute; for a peg's FX the daily point holding that lookback's start. On the
// recorded sources when it has them.
func (j rowJudge) headlineCovered(d *AssetDetail, now time.Time) bool {
	own := now.Add(-assetPriceLookback)
	if j.known && len(d.PriceSources) > 0 {
		return judgeBucket(j.ctx, j.set, []canonical.Pair{{Base: j.asset, Quote: defaultPriceQuote}}, d.PriceSources, own, now) != pricingguard.NotWithheld
	}
	xlm := own.Add(-assetAnchorMaxAge)
	if j.xlm {
		xlm = now.Add(-assetSnapshotMaxAge - assetAnchorMaxAge)
	}
	return j.legsCovered(own, xlm, own.Truncate(24*time.Hour), now)
}

// cleanSeries nulls each point whose bucket an exclusion covers: a point is labelled by its bucket's start, so it holds a value
// read in [its label, the next point's), the last one up to now. A gap stands where it was, never a zero.
func cleanSeries(pts []AssetPricePoint, covered func(from, to time.Time) bool, now time.Time, removed bool) ([]AssetPricePoint, bool) {
	for i := range pts {
		if pts[i].P == nil {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, pts[i].T)
		to := now
		if i+1 < len(pts) {
			if next, nerr := time.Parse(time.RFC3339Nano, pts[i+1].T); nerr == nil && next.After(t) {
				to = next
			}
		}
		if err != nil || covered(t, to) {
			pts[i].P, removed = nil, true
		}
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
