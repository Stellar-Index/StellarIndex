'use client';

import { assetHrefFor } from '@/lib/fiat-slugs';
import { useLastPathSegment } from '@/lib/useLastPathSegment';

import { EmbedShellFrame, EmbedShellMessage } from '../../EmbedShellFrame';
import { LivePrice } from '../../LivePrice';

/**
 * EmbedCurrencyPathView — the runtime fallback for /embed/currency/[ticker]
 * outside the build-time pre-render (a currency added since, or a
 * lowercase ticker the build never emits), served by
 * functions/embed/currency/[[path]].js (T291). The USD rate loads live.
 */
export function EmbedCurrencyPathView() {
  const upper = useLastPathSegment().toUpperCase();
  if (!upper) return <EmbedShellMessage>Loading…</EmbedShellMessage>;

  return (
    <EmbedShellFrame label={upper} sublabel="vs USD" href={assetHrefFor(upper)}>
      <LivePrice assetId={`fiat:${upper}`} initial="—" format="usd" />
    </EmbedShellFrame>
  );
}
