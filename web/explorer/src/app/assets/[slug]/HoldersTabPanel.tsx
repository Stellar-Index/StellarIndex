'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { formatCompact } from '@/lib/format';
import type { paths } from '@/api/types';

// GET /v1/assets/{id}/holders response body from the generated OpenAPI
// contract (src/api/types.ts, `make web-generate-api`).
type HoldersResp = NonNullable<
  paths['/assets/{asset_id}/holders']['get']['responses'][200]['content']['application/json']['data']
>;

/**
 * HoldersTabPanel — top holders of an asset by current trustline balance,
 * plus the total holder count. Client-fetched at runtime (GET
 * /v1/assets/{id}/holders) so it reflects live state rather than a
 * build-time snapshot. Coverage grows with the entry-change capture
 * window; full once the Phase-C backfill lands.
 */
export function HoldersTabPanel({ assetID, decimals = 7 }: { assetID: string; decimals?: number }) {
  const { data, isLoading, isError } = useQuery<HoldersResp>({
    queryKey: ['/v1/assets/{id}/holders', assetID],
    retry: false,
    staleTime: 60_000,
    queryFn: async () => {
      const env = await apiGet<{ data: HoldersResp }>(
        `/v1/assets/${encodeURIComponent(assetID)}/holders`,
        { limit: 100 },
      );
      return env.data;
    },
  });

  const holders = data?.holders ?? [];
  const source = asExample(`/v1/assets/${assetID}/holders`, { limit: 100 });

  return (
    <Panel
      title={data && (data.holder_count ?? 0) > 0 ? `Holders (${formatCompact(data.holder_count ?? 0)})` : 'Holders'}
      hint={holders.length > 0 ? 'top 100 by balance' : undefined}
      source={source}
      bodyClassName="-mx-4"
    >
      {isLoading ? (
        <p className="px-4 text-sm text-ink-muted">Loading holders…</p>
      ) : isError ? (
        <p className="px-4 text-sm text-down-strong">Failed to load holders.</p>
      ) : holders.length === 0 ? (
        <p className="px-4 text-sm text-ink-muted">
          No holders in the captured ledger window yet — asset trustline state
          fills in as the entry-change backfill progresses.
        </p>
      ) : (
        <div className="overflow-x-auto">
          <table className="min-w-full divide-y divide-line text-sm">
            <thead>
              <tr className="text-left text-[11px] uppercase tracking-wider text-ink-muted">
                <th scope="col" className="px-4 py-2">#</th>
                <th scope="col" className="px-4 py-2">Account</th>
                <th scope="col" className="px-4 py-2 text-right">Balance</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-line-subtle">
              {holders.map((h, i) => (
                <tr key={h.account_id} className="hover:bg-surface-muted">
                  <td className="px-4 py-3 font-mono text-xs text-ink-faint">{i + 1}</td>
                  <td className="px-4 py-3">
                    <Link
                      href={`/accounts/${encodeURIComponent(h.account_id ?? '')}/`}
                      className="font-mono text-xs text-brand-600 hover:underline"
                      title={h.account_id}
                    >
                      {(h.account_id ?? '').slice(0, 8)}…{(h.account_id ?? '').slice(-6)}
                    </Link>
                  </td>
                  <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-body">
                    {(Number(h.balance) / 10 ** decimals).toLocaleString(undefined, { maximumFractionDigits: 4 })}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Panel>
  );
}
