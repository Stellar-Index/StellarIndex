'use client';

import { VolumeShare, topWithOther } from '@/components/charts/VolumeShare';

export interface DexVolumeRow {
  name: string;
  volume_24h_usd?: string | null;
}

/**
 * DexVolumeShare — 24h USD volume share across DEXes. DEXes with no
 * valued volume are counted in a note, never drawn as zero; fewer than
 * two valued DEXes compare nothing, so nothing renders.
 */
export function DexVolumeShare({ rows }: { rows: readonly DexVolumeRow[] }) {
  const shareRows = rows.map((r) => ({
    id: r.name,
    label: r.name,
    volume: r.volume_24h_usd,
  }));
  const { pricedCount } = topWithOther(shareRows, rows.length);
  if (pricedCount < 2) return null;
  const unvalued = rows.length - pricedCount;
  return (
    <VolumeShare
      rows={shareRows}
      topN={8}
      noun="DEXes"
      scopeNote={
        unvalued > 0
          ? `${unvalued} DEX${unvalued === 1 ? '' : 'es'} with no valued volume ${unvalued === 1 ? 'is' : 'are'} not drawn.`
          : undefined
      }
    />
  );
}
