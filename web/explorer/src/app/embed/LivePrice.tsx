'use client';

import { useEffect, useState, useSyncExternalStore } from 'react';
import { compareDecimalStrings, formatPriceSmall } from '@/lib/format';
import { pollWhileVisible } from '@/lib/live/visiblePoll';

import { API_BASE_URL as API_BASE, timeoutSignal } from '@/api/client';
import { CURRENT_NETWORK } from '@/lib/networks';

/**
 * LivePrice — client-side price hydration for the embed widgets
 * (a build-time price would read days-stale under a deploy freeze).
 * Renders the baked price immediately, then replaces
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
  const [unpublished, setUnpublished] = useState(false);

  useEffect(() => {
    if (!assetId || !CURRENT_NETWORK.pricing) return; // test nets have no /v1/price
    let cancelled = false;
    async function tick() {
      try {
        const res = await fetch(
          `${API_BASE}/v1/price?asset=${encodeURIComponent(assetId)}&quote=${encodeURIComponent(quote)}`,
          { signal: timeoutSignal() },
        );
        if (!res.ok) {
          // 404/403 is the API declining to publish a live price. The baked
          // figure may rest on a catalogue basis /v1/price never serves, so
          // ask the catalogue whether it still publishes one before keeping it.
          const refused = res.status === 404 || res.status === 403;
          const published =
            refused && quote === 'fiat:USD'
              ? await cataloguePrice(assetId)
              : undefined;
          if (cancelled) return;
          if (published === null) {
            setPrice('—');
            setUnpublished(true);
            publishLivePrice(assetId, quote, null);
            return;
          }
          setUnpublished(false);
          if (published) {
            setPrice(formatLivePrice(published, format));
            publishLivePrice(assetId, quote, published);
          }
          setWithheld(true);
          return;
        }
        const body = (await res.json()) as {
          data?: { price?: string; observed_at?: string };
        };
        if (
          !cancelled &&
          body.data?.price &&
          compareDecimalStrings(body.data.price, '0') != null
        ) {
          setPrice(formatLivePrice(body.data.price, format));
          setAsOf(body.data.observed_at ?? null);
          setWithheld(false);
          setUnpublished(false);
          publishLivePrice(assetId, quote, body.data.price);
        }
      } catch {
        // keep the last known price — the widget must never blank
      }
    }
    void tick();
    const stop = pollWhileVisible(() => void tick(), 60_000);
    return () => {
      cancelled = true;
      stop();
    };
  }, [assetId, format, quote]);

  return (
    <span className="inline-flex items-baseline gap-1.5">
      <span
        className="font-mono text-2xl tabular-nums"
        title={
          unpublished
            ? 'the API publishes no price for this asset right now'
            : withheld
              ? 'live price unavailable — showing the last catalogue price'
            : asOf
              ? `live VWAP · observed ${asOf}`
              : 'as baked at deploy'
        }
      >
        {price}
      </span>
      {(withheld || unpublished) && (
        <span className="text-ink-faint font-mono text-[11px]" role="status">
          {unpublished ? 'not published' : 'stale'}
        </span>
      )}
    </span>
  );
}

// cataloguePrice — the USD price /v1/assets/{id} publishes: a decimal string,
// null when the catalogue publishes none, undefined when it did not answer.
async function cataloguePrice(
  assetId: string,
): Promise<string | null | undefined> {
  try {
    const res = await fetch(
      `${API_BASE}/v1/assets/${encodeURIComponent(assetId)}`,
      { signal: timeoutSignal() },
    );
    if (res.status === 404 || res.status === 403) return null;
    if (!res.ok) return undefined;
    const body = (await res.json()) as { data?: { price_usd?: string | null } };
    const p = body.data?.price_usd;
    return p && compareDecimalStrings(p, '0') === 1 ? p : null;
  } catch {
    return undefined;
  }
}

type LivePriceListener = () => void;
const livePrices = new Map<string, string | null>();
const livePriceListeners = new Set<LivePriceListener>();

function publishLivePrice(assetId: string, quote: string, price: string | null) {
  livePrices.set(`${assetId}|${quote}`, price);
  livePriceListeners.forEach((l) => l());
}

/**
 * useLivePrice — the price the LivePrice on this page last showed for the
 * asset: a decimal string, null once the API publishes none, undefined before
 * the first answer. Lets a second figure follow the headline without a
 * second poll.
 */
export function useLivePrice(
  assetId: string,
  quote = 'fiat:USD',
): string | null | undefined {
  return useSyncExternalStore(
    (l) => {
      livePriceListeners.add(l);
      return () => livePriceListeners.delete(l);
    },
    () => livePrices.get(`${assetId}|${quote}`),
    () => undefined,
  );
}

export function formatLivePrice(
  n: number | string,
  format?: 'usd' | 'plain',
): string {
  const s = formatPriceSmall(n);
  return format === 'plain' ? s : `$${s}`;
}
