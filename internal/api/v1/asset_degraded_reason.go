package v1

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// detailDegradedReason names why an asset-detail response is flagged degraded; "" when no cause here is one of the closed
// set. obs picks the highest-precedence cause.
func detailDegradedReason(d *AssetDetail, homeDomainDegraded, ctxDead bool) string {
	var causes []string
	add := func(on bool, reason string) {
		if on {
			causes = append(causes, reason)
		}
	}
	add(d.issuerDirectoryUnchecked, obs.DegradedCheckUnread)
	add(d.extensionUnread, obs.DegradedSectionDropped)
	add(d.f2ReadFailed || d.sep1Unread || d.decimalsLakeUnread || d.DecimalsUnresolved || homeDomainDegraded, obs.DegradedPartialRead)
	add(d.SupplyStale || d.priceStale, obs.DegradedSourceStale)
	add(d.extensionStale, obs.DegradedProvisional)
	add(ctxDead, obs.DegradedRequestCanceled)
	return obs.HighestDegradedReason(causes...)
}

func newReasonedAssetDetailEntry(d AssetDetail, body []byte, flags Flags, reason string) *assetDetailEntry {
	e := newAssetDetailEntry(d, body, flags)
	e.reason = reason
	return e
}

// noteListDegradedCause records why an asset listing is flagged degraded: a row whose issuer directory was unchecked
// withholds pricing, an unmeasured read leaves the page a lower bound.
func noteListDegradedCause(ctx context.Context, unmeasured bool, rows []AssetDetail) {
	if anyDirectoryUnchecked(rows) {
		obs.NoteDegradedCause(ctx, obs.DegradedCheckUnread)
	}
	if unmeasured {
		obs.NoteDegradedCause(ctx, obs.DegradedPartialRead)
	}
}

// listDegraded reports whether a listing page is degraded and records why, so no degraded exit goes unnamed. lastGood names a
// last-good cause ([listLastGoodReason]); rows merely past their TTL are stale, not degraded.
func listDegraded(ctx context.Context, unmeasured bool, lastGood string, rows []AssetDetail) bool {
	noteListDegradedCause(ctx, unmeasured, rows)
	if lastGood != "" {
		obs.NoteDegradedCause(ctx, lastGood)
	}
	return lastGood != "" || unmeasured || anyDirectoryUnchecked(rows)
}

// listLastGoodReason combines the catalogue rows' refresh verdict ([CachedAssetsReader.listingLastGood]) with a valuation
// read (supply, peg, price) served past its bound, which is a source_stale figure.
func listLastGoodReason(rowsReason string, valuationStale bool) string {
	if !valuationStale {
		return rowsReason
	}
	return obs.HighestDegradedReason(rowsReason, obs.DegradedSourceStale)
}
