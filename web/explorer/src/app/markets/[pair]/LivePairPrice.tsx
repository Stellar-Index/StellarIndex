'use client';

// LivePairPrice — the market-pair headline price, hydrated live
// (RT-2). The pair page is a static export, so pre-fix the Price panel
// showed the build-time VWAP with an "as of <build>" caption — the
// same build-frozen-price class the asset pages had (2026-08-05). The
// baked value still paints first; the browser then re-fetches
// /v1/price on mount + every 60s, and while /v1/price/tip/stream is
// fresh the headline ticks in real time with a direction flash.

import { useChangeSummary } from '@/api/hooks';
import { cn } from '@/lib/cn';
import { formatPriceSmall } from '@/lib/format';
import {
  isFrameStale,
  tipCaveat,
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
  initialChangePct,
}: {
  base: string;
  quote: string;
  /** Build-time price (already a number) — null when the build had none. */
  initialPrice: number | null;
  initialObservedAt: string | null;
  quoteIsUsd: boolean;
  /** Short label appended for non-USD quotes (e.g. "XLM"). */
  quoteSuffix: string;
  /**
   * 24h % change baked at build time from the page's own chart
   * points (last vs 24h-ago). Rendered as the change badge until the
   * live change-summary worker (GET /v1/changes/pair/{base}/{quote})
   * reports a fresher figure — mirrors the asset-sidebar fix (F090):
   * this page is a static export with no client refresh, so a badge
   * built once at deploy and never touched again can point the wrong
   * direction for as long as the tab stays open while the price
   * beside it keeps ticking live.
   */
  initialChangePct?: number | null;
}) {
  // FEC audit A6-5: the 60s poll loop lives in the canonical usePricePoll.
  const poll = usePricePoll({
    asset: base,
    quote,
    initialPrice,
    initialObservedAt,
  });
  const { price, observedAt, polled, stale, withheldTitle, withheldDetail } = poll;

  const tip = useTipStream(base, quote);
  const clock = useLiveClock();
  const tipFresh =
    tip != null && !isFrameStale(clock, tip.receivedAt, TIP_LIVE_STALE_MS);
  const tipPriceStr = tipFresh ? tip.data.data?.price : undefined;
  const tipNumber = tipPriceStr != null ? Number(tipPriceStr) : NaN;
  const tipActive = Number.isFinite(tipNumber) && tipNumber > 0;
  const flash = usePriceFlash(tipActive ? tipPriceStr : undefined);
  const caveat =
    tipActive && tip ? tipCaveat(tip.data.data, tip.data.flags) : null;

  const shown = tipActive ? tipNumber : price;

  // Same live feed the asset-sidebar change pill polls (F090), keyed on
  // this pair instead of a single coin — never a second, independently
  // -stuck 24h figure beside a price that keeps ticking.
  const changeSummary = useChangeSummary('pair', `${base}/${quote}`);
  const liveChangePct = unwrapH24DeltaPct(changeSummary.data);
  const changePct = liveChangePct ?? initialChangePct ?? null;

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
      {changePct != null && Number.isFinite(changePct) && (
        <ChangeBadge pct={changePct} window="24h" />
      )}
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
          live · streaming{caveat && ` · ${caveat}`}
        </span>
      ) : poll.withheld ? (
        // Mirrors the asset-page sibling: a withheld verdict replaces the
        // timestamp caption — "as of <ts>" under a — price implies the
        // server is stale rather than deliberately refusing to quote.
        // The wording is the server's own (GH-772) — never a hardcoded
        // liquidity-only string, which is wrong for e.g. a scam-issuer
        // withhold.
        <span className="text-ink-muted text-xs">
          {withheldDetail ?? withheldTitle ?? 'price withheld'}
        </span>
      ) : (
        observedAt && (
          <span className="text-ink-muted text-xs">
            as of {formatTimestamp(observedAt)}
            {!polled && ' (at build)'}
            {stale && ' · stale'}
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
  if (!(n > 0)) return '—';
  const num = formatPriceSmall(n);
  return quoteIsUsd ? `$${num}` : `${num} ${quoteSuffix}`;
}

function formatTimestamp(iso: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toISOString().replace('T', ' ').slice(0, 19) + ' UTC';
}

// Reads h24_delta_pct off a bare row (what useChangeSummary returns)
// or the {data, as_of, flags} envelope — same either-shape tolerance as
// the asset-sidebar's unwrapChangeSummary, scoped to the one field this badge needs.
function unwrapH24DeltaPct(raw: unknown): number | null {
  if (raw == null || typeof raw !== 'object') return null;
  const maybe = raw as {
    data?: { h24_delta_pct?: number | null };
    h24_delta_pct?: number | null;
  };
  const row =
    maybe.data != null && typeof maybe.data === 'object' ? maybe.data : maybe;
  return typeof row.h24_delta_pct === 'number' ? row.h24_delta_pct : null;
}

function ChangeBadge({ pct, window }: { pct: number; window: string }) {
  const tone =
    pct > 0
      ? 'bg-up-subtle text-up'
      : pct < 0
        ? 'bg-down-subtle text-down'
        : 'bg-surface-subtle text-ink-body';
  const sign = pct > 0 ? '+' : '';
  return (
    <span
      className={`rounded-sm px-2 py-0.5 font-mono text-xs tabular-nums ${tone}`}
    >
      {sign}
      {pct.toFixed(2)}%
      <span className="ml-1 text-[10px] tracking-wider uppercase opacity-70">
        {window}
      </span>
    </span>
  );
}
