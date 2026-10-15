'use client';

import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { LineChart, type LinePoint } from '@/components/charts/LineChart';
import { Callout, Skeleton } from '@/components/ui';
import { apiGet, asExample } from '@/api/client';
import { formatCompact } from '@/lib/format';
import type { paths } from '@/api/types';

type TrustlineHistoryResp = NonNullable<
  paths['/assets/{asset_id}/trustline-history']['get']['responses'][200]['content']['application/json']['data']
>;

/** The asset_id spellings of native XLM, which has no trustlines. */
function isNativeAssetID(assetID: string): boolean {
  return (
    assetID === 'native' ||
    assetID === 'crypto:XLM' ||
    assetID.toUpperCase() === 'XLM'
  );
}

/** Open trustlines at each active day's end, as chart points. */
export function trustlineLine(
  days: TrustlineHistoryResp['days'],
): LinePoint[] {
  return days.map((d) => ({
    time: Date.parse(`${d.date}T00:00:00Z`) / 1000,
    value: d.live,
  }));
}

/** TrustlineHistoryPanel charts the asset's open trustline count per day since genesis. */
export function TrustlineHistoryPanel({ assetID }: { assetID: string }) {
  const native = isNativeAssetID(assetID);
  const path = `/v1/assets/${encodeURIComponent(assetID)}/trustline-history`;
  const { data, isLoading, isError } = useQuery<TrustlineHistoryResp>({
    queryKey: ['/v1/assets/{id}/trustline-history', assetID],
    enabled: !native,
    retry: false,
    staleTime: 10 * 60_000,
    queryFn: async () =>
      (await apiGet<{ data: TrustlineHistoryResp }>(path)).data,
  });
  if (native) return null;

  const days = data?.days ?? [];
  const last = days.at(-1);
  return (
    <Panel
      headingLevel={2}
      title="Trustlines over time"
      hint={last ? `${formatCompact(last.live)} open` : undefined}
      source={asExample(`/v1/assets/${assetID}/trustline-history`)}
    >
      {isLoading ? (
        <div aria-busy="true">
          <span className="sr-only">Loading trustline history</span>
          <Skeleton className="h-[220px] w-full" aria-hidden />
        </div>
      ) : isError ? (
        <Callout tone="bad">Failed to load trustline history.</Callout>
      ) : days.length === 0 ? (
        <p className="text-ink-muted text-sm">
          No trustline has ever been opened to this asset.
        </p>
      ) : (
        <>
          <LineChart
            data={trustlineLine(days)}
            height={220}
            area
            positive
            ariaLabel={`Open trustlines per day for ${assetID}`}
            legend={{
              valueLabel: 'Open trustlines',
              formatValue: (n) => formatCompact(n),
            }}
          />
          <p className="text-ink-faint mt-2 text-[11px]">
            Through ledger {data?.through_ledger.toLocaleString('en-US')}.
            Days with no trustline opened or closed are not plotted.
          </p>
        </>
      )}
    </Panel>
  );
}
