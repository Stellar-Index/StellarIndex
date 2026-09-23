'use client';

import { shortAssetText } from '@/lib/asset-label';
import { useLastPathSegment } from '@/lib/useLastPathSegment';

import { EmbedShellFrame, EmbedShellMessage } from '../../EmbedShellFrame';
import { LiveChangeChip } from '../../LiveChangeChip';
import { LivePrice } from '../../LivePrice';

/**
 * EmbedPairPathView — the runtime fallback for /embed/pair/[base~quote]
 * outside the build-time top-100 pre-render, served by
 * functions/embed/pair/[[path]].js (T291). Price and 24h change load live.
 */
export function EmbedPairPathView() {
  const slug = useLastPathSegment();
  if (!slug) return <EmbedShellMessage>Loading…</EmbedShellMessage>;
  const ix = slug.indexOf('~');
  if (ix <= 0 || ix === slug.length - 1) {
    return <EmbedShellMessage>Invalid pair slug</EmbedShellMessage>;
  }
  const base = slug.slice(0, ix);
  const quote = slug.slice(ix + 1);

  return (
    <EmbedShellFrame
      label={`${shortAssetText(base)} / ${shortAssetText(quote)}`}
      sublabel="Stellar"
      href={`/markets/${encodeURIComponent(slug)}`}
    >
      <LivePrice assetId={base} quote={quote} initial="—" format="plain" />
      <LiveChangeChip
        entityType="pair"
        entityID={`${base}/${quote}`}
        window="24h"
        initialPct={null}
      />
    </EmbedShellFrame>
  );
}
