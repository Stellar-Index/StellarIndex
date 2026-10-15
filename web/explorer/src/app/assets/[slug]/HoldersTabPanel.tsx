'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import {
  Callout,
  Skeleton,
  Table,
  THead,
  TBody,
  TR,
  Th,
  Td,
  LinkRow,
} from '@/components/ui';
import { PANEL_LABEL_CLASS } from '@/components/ui/Page';
import { apiGet, asExample } from '@/api/client';
import {
  DonutChart,
  CATEGORICAL_PALETTE,
} from '@/components/charts/DonutChart';
import {
  formatCompact,
  ratioPct,
  scaleBaseUnits,
  sumDecimalStrings,
  formatUnitsReadable,
} from '@/lib/format';
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
  return (
    assetID === 'native' ||
    assetID === 'crypto:XLM' ||
    assetID.toUpperCase() === 'XLM'
  );
}

export function HoldersTabPanel({
  assetID,
  decimals = 7,
}: {
  assetID: string;
  decimals?: number;
}) {
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
  const shares = servedShares(holders);
  const maxShare = Math.max(0, ...shares.map((v) => v ?? 0));
  const source = asExample(`/v1/assets/${assetID}/holders`, { limit: 100 });

  return (
    <Panel
      headingLevel={2}
      title={
        data && (data.holder_count ?? 0) > 0
          ? `Holders (${formatCompact(data.holder_count ?? 0)})`
          : 'Holders'
      }
      hint={holders.length > 0 ? 'top 100 by balance' : undefined}
      source={source}
      bodyClassName="-mx-4"
    >
      {isLoading ? (
        <div className="space-y-3 px-4" aria-busy="true">
          <span className="sr-only">Loading holders</span>
          <Skeleton className="h-4 w-3/5" aria-hidden />
          <Skeleton className="h-4 w-full" aria-hidden />
          <Skeleton className="h-4 w-4/5" aria-hidden />
        </div>
      ) : isError ? (
        <div className="px-4">
          <Callout tone="bad">Failed to load holders.</Callout>
        </div>
      ) : holders.length === 0 ? (
        <p className="text-ink-muted px-4 text-sm">
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
          <Table>
            <THead>
              <tr>
                <Th>#</Th>
                <Th>Account</Th>
                <Th align="right">Balance</Th>
                <Th
                  align="right"
                  title="Share of the balance held by the served top rows, not of total supply."
                >
                  Share
                </Th>
              </tr>
            </THead>
            <TBody>
              {holders.map((h, i) => (
                <LinkRow
                  href={`/accounts/${encodeURIComponent(h.account_id ?? '')}/`}
                  key={h.account_id}
                >
                  <Td className="text-ink-faint font-mono text-xs">{i + 1}</Td>
                  <Td className="whitespace-nowrap">
                    <Link
                      href={`/accounts/${encodeURIComponent(h.account_id ?? '')}/`}
                      className="text-brand-600 font-mono text-xs hover:underline"
                      title={h.account_id}
                    >
                      {(h.account_id ?? '').slice(0, 8)}…
                      {(h.account_id ?? '').slice(-6)}
                    </Link>
                  </Td>
                  <Td align="right" className="font-mono">
                    {/* Balances are smallest-unit integer strings that can
                        exceed 2^53 (ADR-0003) — BigInt-divide first, never
                        Number(); an absent balance renders "—", not NaN. */}
                    {formatUnitsReadable(h.balance, decimals)}
                  </Td>
                  <Td>
                    <ShareBar pct={shares[i]} max={maxShare} />
                  </Td>
                </LinkRow>
              ))}
            </TBody>
          </Table>
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
/** Each row's percentage of the served rows' summed balance; negatives count as zero. */
export function servedShares(
  holders: { balance?: string }[],
): (number | null)[] {
  const balances = holders.map((h) =>
    h.balance?.trim().startsWith('-') ? '0' : h.balance,
  );
  const total = sumDecimalStrings(balances);
  return balances.map((b) => ratioPct(b, total, 1));
}

function ShareBar({ pct, max }: { pct: number | null; max: number }) {
  if (pct == null) return <span className="text-ink-faint">—</span>;
  const width = max > 0 ? Math.max(1, (pct / max) * 100) : 0;
  return (
    <div
      className="flex items-center justify-end gap-2"
      aria-label={`${pct}% of the served rows`}
    >
      <div className="bg-surface-muted h-1.5 w-24 rounded-full">
        <div
          className="bg-brand-500 h-1.5 rounded-full"
          style={{ width: `${width}%` }}
        />
      </div>
      <span className="text-ink-muted w-12 text-right font-mono text-xs tabular-nums">
        {pct}%
      </span>
    </div>
  );
}

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
  // Negatives clamp to zero as in the donut; malformed input hides the label.
  const balances = holders.map((h) =>
    h.balance?.trim().startsWith('-') ? '0' : h.balance,
  );
  const topPct = ratioPct(
    sumDecimalStrings(balances.slice(0, 10)),
    sumDecimalStrings(balances),
    0,
  );
  return (
    <div className="border-line-subtle border-b px-4 pb-4">
      <h3 className={`${PANEL_LABEL_CLASS} mb-2`}>
        Concentration — within the served top {holders.length}
      </h3>
      <DonutChart
        data={[
          {
            label: 'Top 10 holders',
            value: top10,
            color: CATEGORICAL_PALETTE[0],
          },
          {
            label: `Ranks 11–${holders.length}`,
            value: rest,
            color: CATEGORICAL_PALETTE[CATEGORICAL_PALETTE.length - 1],
          },
        ]}
        size={128}
        thickness={18}
        centerLabel={topPct == null ? undefined : `${topPct}%`}
        centerSub="top 10"
        formatValue={(n) => formatCompact(n)}
      />
      <p
        className="text-ink-faint mt-2 text-xs"
        title="Share of the balance held by the served top rows only, not of total supply (the full holder set isn’t served here)."
      >
        Share of the top-{holders.length} served rows only, not of supply.
      </p>
    </div>
  );
}
