'use client';

import { useEffect, useState } from 'react';
import { formatSubunitPrice } from '@/lib/format';

import { API_BASE_URL as API_BASE } from '@/api/client';

/**
 * LivePrice — client-side price hydration for the embed widgets
 * (Pass-B AM-14: the embeds were fully static, selling a build-time
 * price as a live ticker; under a deploy freeze they read days-stale
 * with no hint). Renders the baked price immediately, then replaces
 * it with the live VWAP and keeps it fresh on a 60s cadence. The API
 * is CORS-open for reads, so this works from any embedding origin.
 */
export function LivePrice({
  assetId,
  initial,
  format,
  quote = 'fiat:USD',
}: {
  assetId: string;
  initial: string;
  format?: 'usd' | 'plain';
  // The quote asset to price `assetId` against. Defaults to fiat:USD so
  // the USD-denominated embeds (asset, currency) need not pass it; the
  // pair embed passes the pair's own quote so it refreshes the same
  // base/quote price it baked, not a USD conversion.
  quote?: string;
}) {
  const [price, setPrice] = useState(initial);
  const [asOf, setAsOf] = useState<string | null>(null);
  const [withheld, setWithheld] = useState(false);

  useEffect(() => {
    let cancelled = false;
    async function tick() {
      try {
        const res = await fetch(
          `${API_BASE}/v1/price?asset=${encodeURIComponent(assetId)}&quote=${encodeURIComponent(quote)}`,
        );
        if (!res.ok) {
          // 404/403: the API is deliberately withholding the price (not a
          // network blip). Say so visibly — a tooltip alone left the
          // build-time price looking live with no on-screen hint.
          if (!cancelled) setWithheld(true);
          return;
        }
        const body = (await res.json()) as {
          data?: { price?: string; observed_at?: string };
        };
        if (!cancelled && body.data?.price) {
          setPrice(formatLivePrice(Number(body.data.price), format));
          setAsOf(body.data.observed_at ?? null);
          setWithheld(false);
        }
      } catch {
        // keep the last known price — the widget must never blank
      }
    }
    void tick();
    const t = setInterval(tick, 60_000);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, [assetId, format, quote]);

  return (
    <span className="inline-flex items-baseline gap-1.5">
      <span
        className="font-mono text-2xl tabular-nums"
        title={
          withheld
            ? 'live price unavailable — showing the price baked at deploy'
            : asOf
              ? `live VWAP · observed ${asOf}`
              : 'as baked at deploy'
        }
      >
        {price}
      </span>
      {withheld && (
        <span className="text-ink-faint font-mono text-[11px]" role="status">
          stale
        </span>
      )}
    </span>
  );
}

export function formatLivePrice(n: number, format?: 'usd' | 'plain'): string {
  const s =
    n >= 1
      ? n.toFixed(n >= 100 ? 2 : 4)
      : n >= 0.001
        ? n.toFixed(6)
        : formatSubunitPrice(n);
  return format === 'plain' ? s : `$${s}`;
}
