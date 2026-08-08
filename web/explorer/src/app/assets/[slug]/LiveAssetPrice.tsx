'use client';

import { useEffect, useState } from 'react';

import { API_BASE_URL } from '@/api/client';
import { isFrameStale, useLiveClock, usePriceFlash, useTipStream } from '@/lib/live/hooks';
import { cn } from '@/lib/cn';
import { formatPriceSmall } from '@/lib/format';

export type PriceProvenance = 'vwap1m' | 'triangulated' | 'listing' | null;

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
  const [price, setPrice] = useState<number | null>(initialPrice);
  const [provenance, setProvenance] = useState<PriceProvenance>(initialProvenance);
  const [stale, setStale] = useState<boolean>(Boolean(initialStale));
  const [live, setLive] = useState(false);
  const [withheld, setWithheld] = useState(false);

  // Tip stream (disabled once the server says withheld — the stream's
  // own pre-flight would keep 404ing anyway).
  const tip = useTipStream(withheld ? null : assetID);
  // Slow clock so a wedged stream loses its "live" claim without
  // waiting for the next poll render (WB-04).
  const clock = useLiveClock();
  const tipFresh = tip != null && !isFrameStale(clock, tip.receivedAt, TIP_LIVE_STALE_MS);
  const tipPriceStr = tipFresh ? tip.data.data?.price : undefined;
  const tipNumber = tipPriceStr != null ? Number(tipPriceStr) : NaN;
  const tipActive = Number.isFinite(tipNumber) && tipNumber > 0;
  const flash = usePriceFlash(tipActive ? tipPriceStr : undefined);

  useEffect(() => {
    let cancelled = false;
    const tick = async () => {
      try {
        const r = await fetch(
          `${API_BASE_URL}/v1/price?asset=${encodeURIComponent(assetID)}&quote=fiat:USD`,
        );
        if (cancelled) return;
        if (r.status === 404) {
          const body = (await r.json().catch(() => null)) as { type?: string } | null;
          if (body?.type?.endsWith('/price-withheld')) {
            setWithheld(true);
            setPrice(null);
            setLive(true);
          }
          return; // plain not-found: keep the baked value + caption
        }
        if (!r.ok) return;
        const body = (await r.json()) as {
          data?: { price?: string };
          flags?: { stale?: boolean; triangulated?: boolean };
        };
        const n = Number(body.data?.price);
        if (!Number.isFinite(n) || n <= 0) return;
        setPrice(n);
        setProvenance(body.flags?.triangulated ? 'triangulated' : 'vwap1m');
        setStale(Boolean(body.flags?.stale));
        setWithheld(false);
        setLive(true);
      } catch {
        // Network blip — keep whatever we have.
      }
    };
    void tick();
    const id = setInterval(() => void tick(), 60_000);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [assetID]);

  const shown = tipActive ? tipNumber : price;

  return (
    <>
      <div className="mt-3 flex flex-wrap items-baseline gap-2">
        <span
          className={cn(
            'font-mono text-3xl tabular-nums text-ink',
            flash === 'up' && 'flash-up',
            flash === 'down' && 'flash-down',
          )}
        >
          {shown != null ? `$${formatPriceSmall(shown)}` : '—'}
        </span>
        {changePill}
        {tipActive && (
          <span className="relative flex h-2 w-2" aria-label="live" role="status">
            <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-up opacity-60" />
            <span className="relative inline-flex h-2 w-2 rounded-full bg-up" />
          </span>
        )}
      </div>
      <p className="mt-1 text-[11px] uppercase tracking-wider text-ink-muted">
        {withheld && 'price withheld · market too thin to aggregate'}
        {!withheld && tipActive && 'live tip price · USD · streaming'}
        {!withheld && !tipActive && shown != null && provenance === 'vwap1m' && '1-min VWAP · USD'}
        {!withheld && !tipActive && shown != null && provenance === 'triangulated' && '1-min VWAP · USD · triangulated via XLM'}
        {!withheld && !tipActive && shown != null && provenance === 'listing' && 'listing snapshot · not a live aggregated price'}
        {!withheld && !tipActive && shown != null && stale && ' · stale'}
        {!withheld && !tipActive && shown != null && !live && provenance !== 'listing' && ' · as baked at deploy'}
      </p>
    </>
  );
}
