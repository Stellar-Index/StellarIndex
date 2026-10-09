'use client';

import { HBarList } from '@/components/charts/Bars';
import { compareDecimalStrings, formatCompactUnits } from '@/lib/format';

export interface RoutedVolumeRow {
  contract_id: string;
  name: string;
  kind: string;
  routed_volume_24h_usd: string | null;
}

/**
 * RoutedVolumeBars — routers ranked by 24h routed USD volume. Routers with
 * no USD valuation yet are counted in a note, never drawn as zero; vaults
 * never route trades and are left out. Fewer than two valued routers
 * compare nothing, so the chart is omitted.
 */
export function RoutedVolumeBars({
  rows,
}: {
  rows: readonly RoutedVolumeRow[];
}) {
  const routers = rows.filter((r) => r.kind === 'router');
  const valued = routers
    .filter(
      (r): r is RoutedVolumeRow & { routed_volume_24h_usd: string } =>
        r.routed_volume_24h_usd != null &&
        compareDecimalStrings(r.routed_volume_24h_usd, '0') === 1,
    )
    .sort(
      (a, b) =>
        -(
          compareDecimalStrings(
            a.routed_volume_24h_usd,
            b.routed_volume_24h_usd,
          ) ?? 0
        ),
    );
  if (valued.length < 2) return null;
  const unvalued = routers.length - valued.length;

  return (
    <div className="space-y-2" data-testid="routed-volume-bars">
      <HBarList
        ariaLabel="Routers ranked by 24h routed USD volume"
        items={valued.map((r) => ({
          id: r.contract_id,
          label: r.name,
          value: Number(r.routed_volume_24h_usd),
          display: `$${formatCompactUnits(r.routed_volume_24h_usd)}`,
        }))}
      />
      <p className="text-ink-muted text-xs">
        Valued routed trades only.
        {unvalued > 0
          ? ` ${unvalued} router${unvalued === 1 ? '' : 's'} with no USD valuation yet ${unvalued === 1 ? 'is' : 'are'} not drawn.`
          : ''}
      </p>
    </div>
  );
}
