'use client';

import { useQuery } from '@tanstack/react-query';

import type { Coin } from '@/api/hooks';
import { apiGetData } from '@/api/client';
import { assetHref } from '@/lib/fiat-slugs';
import { useLastPathSegment } from '@/lib/useLastPathSegment';

import { EmbedShellFrame, EmbedShellMessage } from '../../EmbedShellFrame';
import { LiveChangeChip } from '../../LiveChangeChip';
import { LivePrice, formatLivePrice } from '../../LivePrice';

/**
 * EmbedAssetPathView — the runtime fallback for /embed/asset/[slug]
 * outside the build-time pre-render, served by
 * functions/embed/asset/[[path]].js (T291). Resolves the slug through
 * /v1/assets/{slug} (XLM → native, as the baked page does) for the
 * canonical asset id the live price and change chip key on.
 */
export function EmbedAssetPathView() {
  const slug = useLastPathSegment();
  const norm = slug.toLowerCase();
  const id = norm === 'xlm' || norm === 'native' ? 'native' : slug;
  const q = useQuery<Coin>({
    queryKey: ['/v1/assets', id],
    queryFn: () => apiGetData<Coin>(`/v1/assets/${encodeURIComponent(id)}`),
    enabled: slug !== '',
    retry: false,
  });

  if (q.isError)
    return <EmbedShellMessage>No data for {slug}</EmbedShellMessage>;
  if (!q.data) return <EmbedShellMessage>Loading…</EmbedShellMessage>;

  const coin = q.data;
  const code = coin.code ?? (coin as { ticker?: string }).ticker ?? slug;
  const price = coin.price_usd ? Number(coin.price_usd) : null;
  const initial =
    price != null && Number.isFinite(price) && price > 0
      ? formatLivePrice(price, 'usd')
      : '—';
  const change24h = coin.change_24h_pct ? Number(coin.change_24h_pct) : null;
  const liveId = coin.asset_id ?? '';

  return (
    <EmbedShellFrame label={code} sublabel="Stellar" href={assetHref(slug)}>
      {liveId ? (
        <LivePrice assetId={liveId} initial={initial} />
      ) : (
        <span className="font-mono text-2xl tabular-nums">{initial}</span>
      )}
      {liveId && (
        <LiveChangeChip
          entityType="coin"
          entityID={liveId}
          window="24h"
          initialPct={change24h}
        />
      )}
    </EmbedShellFrame>
  );
}
