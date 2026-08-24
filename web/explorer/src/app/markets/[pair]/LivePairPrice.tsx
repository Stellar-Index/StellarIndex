'use client';

// LivePairPrice — the market-pair headline price, hydrated live
// (RT-2). The pair page is a static export, so pre-fix the Price panel
// showed the build-time VWAP with an "as of <build>" caption — the
// same build-frozen-price class the asset pages had (2026-08-05). The
// baked value still paints first; the browser then re-fetches
// /v1/price on mount + every 60s, and while /v1/price/tip/stream is
// fresh the headline ticks in real time with a direction flash.

import { cn } from '@/lib/cn';
import { formatSubunitPrice } from '@/lib/format';
import {
  isFrameStale,
  useLiveClock,
  usePriceFlash,
  usePricePoll,
  useTipStream,
} from '@/lib/live/hooks';

const TIP_LIVE_STALE_MS = 30_000;

export function LivePairPrice({
  base,
  quote,
  initialPrice,
  initialObservedAt,
  quoteIsUsd,
  quoteSuffix,
}: {
  base: string;
  quote: string;
  /** Build-time price (already a number) — null when the build had none. */
  initialPrice: number | null;
  initialObservedAt: string | null;
  quoteIsUsd: boolean;
  /** Short label appended for non-USD quotes (e.g. "XLM"). */
  quoteSuffix: string;
}) {
  // FEC audit A6-5: the 60s poll loop lives in the canonical usePricePoll.
  const poll = usePricePoll({
    asset: base,
    quote,
    initialPrice,
    initialObservedAt,
  });
  const { price, observedAt, polled } = poll;

  const tip = useTipStream(base, quote);
  const clock = useLiveClock();
  const tipPriceStr =
    tip != null && !isFrameStale(clock, tip.receivedAt, TIP_LIVE_STALE_MS)
      ? tip.data.data?.price
      : undefined;
  const tipNumber = tipPriceStr != null ? Number(tipPriceStr) : NaN;
  const tipActive = Number.isFinite(tipNumber) && tipNumber > 0;
  const flash = usePriceFlash(tipActive ? tipPriceStr : undefined);

  const shown = tipActive ? tipNumber : price;

  return (
    <>
      <span
        className={cn(
          'font-mono text-3xl tabular-nums',
          flash === 'up' && 'flash-up',
          flash === 'down' && 'flash-down',
        )}
      >
        {shown != null ? formatQuotePrice(shown, quoteIsUsd, quoteSuffix) : '—'}
      </span>
      {tipActive ? (
        <span className="text-ink-muted flex items-center gap-1.5 text-xs">
          <span
            className="relative flex h-2 w-2"
            aria-label="live"
            role="status"
          >
            <span className="bg-up absolute inline-flex h-full w-full animate-ping rounded-full opacity-60" />
            <span className="bg-up relative inline-flex h-2 w-2 rounded-full" />
          </span>
          live · streaming
        </span>
      ) : poll.withheld ? (
        // Mirrors the asset-page sibling: a withheld verdict replaces the
        // timestamp caption — "as of <ts>" under a — price implies the
        // server is stale rather than deliberately refusing to quote.
        <span className="text-ink-muted text-xs">
          price withheld · market too thin to aggregate
        </span>
      ) : (
        observedAt && (
          <span className="text-ink-muted text-xs">
            as of {formatTimestamp(observedAt)}
            {!polled && ' (at build)'}
          </span>
        )
      )}
    </>
  );
}

function formatQuotePrice(
  n: number,
  quoteIsUsd: boolean,
  quoteSuffix: string,
): string {
  const num =
    n >= 1
      ? n.toFixed(n >= 100 ? 2 : 4)
      : n >= 0.001
        ? n.toFixed(6)
        : n > 0
          ? formatSubunitPrice(n)
          : '—';
  if (num === '—') return num;
  return quoteIsUsd ? `$${num}` : `${num} ${quoteSuffix}`;
}

function formatTimestamp(iso: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toISOString().replace('T', ' ').slice(0, 19) + ' UTC';
}
