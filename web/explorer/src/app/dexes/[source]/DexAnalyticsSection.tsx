'use client';

import { asExample } from '@/api/client';
import { BespokeSection } from '@/app/protocols/[name]/BespokeSection';
import { isUsdVolumeSeries, useDexBespoke } from './SourceVolumeHistory';

/**
 * DexAnalyticsSection — the per-DEX bespoke analytics suite on
 * /dexes/[source]: KPI cards, trades + unique-traders series, the
 * top-pairs multi-line panel, the "Volume by pair" donut, and the bespoke
 * tables — all from the /v1/protocols/{source} bespoke block the page's
 * volume panel already fetches (one shared query). Rendering is delegated
 * wholesale to BespokeSection (window pills included) so the venue page
 * and the protocol page stay one implementation.
 *
 * The standalone "USD volume" series is omitted here — SourceVolumeHistory
 * renders it as its own panel on this page — and the whole section is
 * absent (not zero-filled) when the API serves no bespoke block.
 */
export function DexAnalyticsSection({ source }: { source: string }) {
  const q = useDexBespoke(source);
  const bespoke = q.data;
  if (!bespoke) return null;

  return (
    <BespokeSection
      bespoke={bespoke}
      source={asExample(`/v1/protocols/${source}`)}
      name={source}
      omitSeries={isUsdVolumeSeries}
    />
  );
}
