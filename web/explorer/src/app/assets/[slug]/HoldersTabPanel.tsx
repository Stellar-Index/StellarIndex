'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { DonutChart, CATEGORICAL_PALETTE } from '@/components/charts/DonutChart';
import { formatBaseUnits, formatCompact, scaleBaseUnits } from '@/lib/format';
import type { paths } from '@/api/types';

// GET /v1/assets/{id}/holders response body from the generated OpenAPI
// contract (src/api/types.ts, `make web-generate-api`).
type HoldersResp = NonNullable<
  paths['/assets/{asset_id}/holders']['get']['responses'][200]['content']['application/json']['data']
>;

/**
 * HoldersTabPanel — top holders of an asset by current balance, plus the
 * total holder count. Issued assets rank by trustline balance; native XLM
 * (which has no trustlines) ranks by account balance, so its holder count
 * is the number of funded accounts. Client-fetched at runtime (GET
 * /v1/assets/{id}/holders) so it reflects live state rather than a
 * build-time snapshot. Coverage grows with the entry-change capture
 * window; full once the Phase-C backfill lands.
 */

/** The asset_id spellings that serve the native XLM (account-balance) board. */
function isNativeAssetID(assetID: string): boolean {
  return assetID === 'native' || assetID === 'crypto:XLM' || assetID.toUpperCase() === 'XLM';
}

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
          {isNativeAssetID(assetID)
            ? // Native has no trustlines — its board ranks account balances,
              // so an empty result here is a warming/availability state, NOT
              // a backfill gap. The old copy blamed "trustline state …
              // backfill" for XLM, which was simply false.
              'XLM holder data is not available right now — the account-balance ranking may still be warming; retry shortly.'
            : 'No holders in the captured ledger window yet — asset trustline state fills in as the entry-change backfill progresses.'}
        </p>
      ) : (
        <div className="overflow-x-auto">
          <HoldersConcentration holders={holders} decimals={decimals} />
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
                    {/* Balances are smallest-unit integer strings that can
                        exceed 2^53 (ADR-0003) — BigInt-divide first, never
                        Number(); an absent balance renders "—", not NaN. */}
                    {formatBaseUnits(h.balance, decimals)}
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

/**
 * HoldersConcentration — top-10 share of the SERVED holder rows, as a
 * two-slice donut. The endpoint returns the top-N (100) accounts by
 * balance plus an exact holder_count, NOT every balance — so this is
 * concentration within the served top rows only, and both the title and
 * hint say so rather than implying a full-supply share. Skipped when 10
 * or fewer rows are served (top-10 vs itself is meaningless).
 */
function HoldersConcentration({
  holders,
  decimals,
}: {
  holders: { account_id?: string; balance?: string }[];
  decimals: number;
}) {
  if (holders.length <= 10) return null;
  // Donut geometry only — but still BigInt-divide-first (scaleBaseUnits)
  // so a >2^53 whale balance isn't rounded before scaling (ADR-0003).
  const scaled = holders.map((h) => {
    const n = scaleBaseUnits(h.balance, decimals);
    return n != null && n > 0 ? n : 0;
  });
  const top10 = scaled.slice(0, 10).reduce((a, b) => a + b, 0);
  const rest = scaled.slice(10).reduce((a, b) => a + b, 0);
  const total = top10 + rest;
  if (total <= 0) return null;
  return (
    <div className="border-b border-line-subtle px-4 pb-4">
      <h4 className="mb-2 text-[11px] uppercase tracking-wider text-ink-muted">
        Concentration — within the served top {holders.length}
      </h4>
      <DonutChart
        data={[
          { label: 'Top 10 holders', value: top10, color: CATEGORICAL_PALETTE[0] },
          {
            label: `Ranks 11–${holders.length}`,
            value: rest,
            color: CATEGORICAL_PALETTE[CATEGORICAL_PALETTE.length - 1],
          },
        ]}
        size={128}
        thickness={18}
        centerLabel={`${((top10 / total) * 100).toFixed(0)}%`}
        centerSub="top 10"
        formatValue={(n) => formatCompact(n)}
      />
      <p className="mt-2 text-[11px] text-ink-faint">
        Share of the balance held by the served top-{holders.length} rows only —
        not of total supply (the full holder set isn&rsquo;t served here).
      </p>
    </div>
  );
}
