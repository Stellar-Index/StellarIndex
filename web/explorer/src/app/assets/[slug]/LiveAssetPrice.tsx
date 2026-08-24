'use client';

import {
  isFrameStale,
  useLiveClock,
  usePriceFlash,
  usePricePoll,
  useTipStream,
} from '@/lib/live/hooks';
import { cn } from '@/lib/cn';
import { formatPriceSmall } from '@/lib/format';

export type PriceProvenance =
  | 'vwap1m'
  | 'triangulated'
  | 'listing'
  | 'declared_peg'
  | null;

/** A tip tick is "live" while fresher than this (producer window is
 * 5s; 30s of silence = wedged stream or backgrounded tab). */
const TIP_LIVE_STALE_MS = 30_000;

// LiveAssetPrice — the sidebar headline price, hydrated LIVE.
//
// 2026-08-05: the asset pages are a static export with NO client price
// refresh, so the baked number drifts until the next deploy — the
// operator caught XLM showing a 12-day-old $0.186 against a live $0.17
// chart. The static build still bakes an initial value (so the page
// paints instantly and search engines see a number), but the browser
// re-fetches /v1/price on mount and every 60s, replacing the baked
// figure and re-stamping the provenance caption.
//
// 2026-08-08 (live-tick program RT-2): on top of the poll, the browser
// subscribes to /v1/price/tip/stream via the shared SSE multiplexer.
// While tip frames are fresh they take over the headline — every tick
// re-renders the number with a direction flash, and the caption
// switches to the live-tip provenance. If the stream is unavailable
// (older API, blocked SSE, substance-withheld pair → pre-flight 404
// hard-closes it), everything degrades to the poll shape above. On
// fetch failure the baked value stays, still captioned with its
// provenance — never a blank flash, never a silent stale number
// presented as live.
//
// A 404 with problem type …/price-withheld REPLACES any baked price
// with the withheld verdict: the server has deliberately refused to
// aggregate this market, and this page must not keep showing a
// lower-trust snapshot in its place.
export function LiveAssetPrice({
  assetID,
  initialPrice,
  initialProvenance,
  initialStale,
  changePill,
}: {
  assetID: string;
  initialPrice: number | null;
  initialProvenance: PriceProvenance;
  initialStale?: boolean;
  /** Rendered beside the price (the 24h change pill — server-derived). */
  changePill?: React.ReactNode;
}) {
  // FEC audit A6-5: the 60s poll loop lives in the canonical usePricePoll;
  // this component keeps only the provenance/caption mapping.
  const poll = usePricePoll({ asset: assetID, initialPrice });
  // Declared-peg exception to the withheld-replaces-baked rule: for a
  // peg-basis row the /v1/price withheld verdict is EXPECTED — the
  // server refuses the (dust-authored) MARKET price while the assets
  // surface serves the operator-declared peg basis, which is not a
  // market claim and therefore not the "lower-trust snapshot" the rule
  // exists to purge. Keep the peg price + its honest caption instead
  // of blanking it on every poll.
  const pegged = initialProvenance === 'declared_peg';
  const withheld = poll.withheld && !pegged;
  const price = poll.withheld && pegged ? initialPrice : poll.price;
  const live = poll.polled;
  const stale = poll.polled ? poll.stale : Boolean(initialStale);
  const provenance: PriceProvenance =
    poll.polled && !poll.withheld
      ? poll.triangulated
        ? 'triangulated'
        : 'vwap1m'
      : initialProvenance;

  // Tip stream (disabled once the server says withheld — the stream's
  // own pre-flight would keep 404ing anyway).
  const tip = useTipStream(withheld ? null : assetID);
  // Slow clock so a wedged stream loses its "live" claim without
  // waiting for the next poll render (WB-04).
  const clock = useLiveClock();
  const tipFresh =
    tip != null && !isFrameStale(clock, tip.receivedAt, TIP_LIVE_STALE_MS);
  const tipPriceStr = tipFresh ? tip.data.data?.price : undefined;
  const tipNumber = tipPriceStr != null ? Number(tipPriceStr) : NaN;
  const tipActive = Number.isFinite(tipNumber) && tipNumber > 0;
  const flash = usePriceFlash(tipActive ? tipPriceStr : undefined);

  const shown = tipActive ? tipNumber : price;

  return (
    <>
      <div className="mt-3 flex flex-wrap items-baseline gap-2">
        <span
          className={cn(
            'text-ink font-mono text-3xl tabular-nums',
            flash === 'up' && 'flash-up',
            flash === 'down' && 'flash-down',
          )}
        >
          {shown != null ? `$${formatPriceSmall(shown)}` : '—'}
        </span>
        {changePill}
        {tipActive && (
          <span
            className="relative flex h-2 w-2"
            aria-label="live"
            role="status"
          >
            <span className="bg-up absolute inline-flex h-full w-full animate-ping rounded-full opacity-60" />
            <span className="bg-up relative inline-flex h-2 w-2 rounded-full" />
          </span>
        )}
      </div>
      <p className="text-ink-muted mt-1 text-[11px] tracking-wider uppercase">
        {withheld && 'price withheld · market too thin to aggregate'}
        {!withheld && tipActive && 'live tip price · USD · streaming'}
        {!withheld &&
          !tipActive &&
          shown != null &&
          provenance === 'vwap1m' &&
          '1-min VWAP · USD'}
        {!withheld &&
          !tipActive &&
          shown != null &&
          provenance === 'triangulated' &&
          '1-min VWAP · USD · triangulated via XLM'}
        {!withheld &&
          !tipActive &&
          shown != null &&
          provenance === 'listing' &&
          'listing snapshot · not a live aggregated price'}
        {!withheld &&
          !tipActive &&
          shown != null &&
          provenance === 'declared_peg' &&
          'pegged · declared 1:1 fiat peg × fx rate · not a market price'}
        {!withheld && !tipActive && shown != null && stale && ' · stale'}
        {!withheld &&
          !tipActive &&
          shown != null &&
          !live &&
          provenance !== 'listing' &&
          provenance !== 'declared_peg' &&
          ' · as baked at deploy'}
      </p>
    </>
  );
}
