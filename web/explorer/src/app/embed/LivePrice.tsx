'use client';

import { useEffect, useState } from 'react';

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
}: {
  assetId: string;
  initial: string;
  format?: 'usd' | 'plain';
}) {
  const [price, setPrice] = useState(initial);
  const [asOf, setAsOf] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    async function tick() {
      try {
        const res = await fetch(
          `${API_BASE}/v1/price?asset=${encodeURIComponent(assetId)}&quote=fiat:USD`,
        );
        if (!res.ok) return;
        const body = (await res.json()) as {
          data?: { price?: string; observed_at?: string };
        };
        if (!cancelled && body.data?.price) {
          setPrice(fmt(Number(body.data.price), format));
          setAsOf(body.data.observed_at ?? null);
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
  }, [assetId, format]);

  return (
    <span
      className="font-mono text-2xl tabular-nums"
      title={asOf ? `live VWAP · observed ${asOf}` : 'as baked at deploy'}
    >
      {price}
    </span>
  );
}

function fmt(n: number, format?: 'usd' | 'plain'): string {
  const s =
    n >= 1 ? n.toFixed(n >= 100 ? 2 : 4) : n >= 0.001 ? n.toFixed(6) : n.toExponential(3);
  return format === 'plain' ? s : `$${s}`;
}
