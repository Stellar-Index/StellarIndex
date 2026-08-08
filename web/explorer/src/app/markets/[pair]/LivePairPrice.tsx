'use client';

// LivePairPrice — the market-pair headline price, hydrated live
// (RT-2). The pair page is a static export, so pre-fix the Price panel
// showed the build-time VWAP with an "as of <build>" caption — the
// same build-frozen-price class the asset pages had (2026-08-05). The
// baked value still paints first; the browser then re-fetches
// /v1/price on mount + every 60s, and while /v1/price/tip/stream is
// fresh the headline ticks in real time with a direction flash.
import { useEffect, useState } from 'react';

import { API_BASE_URL } from '@/api/client';
import { cn } from '@/lib/cn';
import { isFrameStale, useLiveClock, usePriceFlash, useTipStream } from '@/lib/live/hooks';

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
  const [price, setPrice] = useState<number | null>(initialPrice);
  const [observedAt, setObservedAt] = useState<string | null>(initialObservedAt);
  const [polled, setPolled] = useState(false);

  const tip = useTipStream(base, quote);
  const clock = useLiveClock();
  const tipPriceStr =
    tip != null && !isFrameStale(clock, tip.receivedAt, TIP_LIVE_STALE_MS)
      ? tip.data.data?.price
      : undefined;
  const tipNumber = tipPriceStr != null ? Number(tipPriceStr) : NaN;
  const tipActive = Number.isFinite(tipNumber) && tipNumber > 0;
  const flash = usePriceFlash(tipActive ? tipPriceStr : undefined);

  useEffect(() => {
    let cancelled = false;
    const tick = async () => {
      try {
        const r = await fetch(
          `${API_BASE_URL}/v1/price?asset=${encodeURIComponent(base)}&quote=${encodeURIComponent(quote)}`,
        );
        if (cancelled || !r.ok) return;
        const body = (await r.json()) as {
          data?: { price?: string; observed_at?: string };
        };
        const n = Number(body.data?.price);
        if (!Number.isFinite(n) || n <= 0) return;
        setPrice(n);
        setObservedAt(body.data?.observed_at ?? null);
        setPolled(true);
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
  }, [base, quote]);

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
        <span className="flex items-center gap-1.5 text-xs text-ink-muted">
          <span className="relative flex h-2 w-2" aria-label="live" role="status">
            <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-up opacity-60" />
            <span className="relative inline-flex h-2 w-2 rounded-full bg-up" />
          </span>
          live · streaming
        </span>
      ) : (
        observedAt && (
          <span className="text-xs text-ink-muted">
            as of {formatTimestamp(observedAt)}
            {!polled && ' (at build)'}
          </span>
        )
      )}
    </>
  );
}

function formatQuotePrice(n: number, quoteIsUsd: boolean, quoteSuffix: string): string {
  const num =
    n >= 1 ? n.toFixed(n >= 100 ? 2 : 4) : n >= 0.001 ? n.toFixed(6) : n > 0 ? n.toExponential(3) : '—';
  if (num === '—') return num;
  return quoteIsUsd ? `$${num}` : `${num} ${quoteSuffix}`;
}

function formatTimestamp(iso: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toISOString().replace('T', ' ').slice(0, 19) + ' UTC';
}
