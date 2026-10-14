'use client';

import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
// /v1/sources row from the generated OpenAPI contract, via the shared
// alias in src/api/hooks.ts.
import type { Source as SourceRow } from '@/api/hooks';
import { Skeleton, TBody, TR, Table, Td, Th, THead } from '@/components/ui';

const NAMES: Record<string, string> = {
  coingecko: 'CoinGecko',
  coingecko_daily: 'CoinGecko (daily)',
  coinmarketcap: 'CoinMarketCap',
  cryptocompare: 'CryptoCompare',
};

const HOMEPAGES: Record<string, string> = {
  coingecko: 'https://www.coingecko.com',
  coingecko_daily: 'https://www.coingecko.com',
  coinmarketcap: 'https://coinmarketcap.com',
  cryptocompare: 'https://www.cryptocompare.com',
};

/**
 * ReferencePriceAggregators — surfaces the data-aggregator class
 * sources (CG / CMC / CryptoCompare) we cross-check against.
 * Distinct from on-chain routers / yield vaults on this page;
 * these don't price into VWAP either, but for a different reason
 * (they're aggregates of upstream venues we already index).
 */
export function ReferencePriceAggregators() {
  const q = useQuery<SourceRow[]>({
    queryKey: ['/v1/sources', 'class=aggregator'],
    queryFn: async () => {
      const env = await apiGet<{ data: SourceRow[] }>('/v1/sources', {
        class: 'aggregator',
        include: 'stats',
      });
      return (env.data ?? []).sort((a, b) => a.name.localeCompare(b.name));
    },
    refetchInterval: 60_000,
  });

  const rows = q.data ?? [];
  // Absent (fetch failed) ≠ empty (none registered) — see the sweep note
  // in src/components/NetworkInsight.tsx.
  const registryAvailable = q.data != null;

  return (
    <Panel
      headingLevel={2}
      title="Reference price aggregators"
      hint="Cross-check sources for divergence detection; never priced into our VWAP"
      source={asExample('/v1/sources', {
        class: 'aggregator',
        include: 'stats',
      })}
      bodyClassName="-mx-4 -mb-4"
    >
      <div className="overflow-x-auto">
        <Table>
          <THead>
            <tr>
              <Th>Source</Th>
              <Th>Cost</Th>
              <Th>Backfill</Th>
            </tr>
          </THead>
          <TBody>
            {q.isLoading && (
              <tr>
                <td colSpan={3} className="px-4 py-3">
                  <Skeleton className="h-5 w-full" />
                </td>
              </tr>
            )}
            {q.isError && (
              <tr>
                <td
                  colSpan={3}
                  className="text-ink-muted px-4 py-6 text-center text-sm"
                >
                  Source registry unavailable right now.
                </td>
              </tr>
            )}
            {registryAvailable && rows.length === 0 && (
              <tr>
                <td
                  colSpan={3}
                  className="text-ink-muted px-4 py-6 text-center text-sm"
                >
                  No reference aggregators registered.
                </td>
              </tr>
            )}
            {rows.map((s) => (
              <TR key={s.name}>
                <Td>
                  {HOMEPAGES[s.name] ? (
                    <a
                      href={HOMEPAGES[s.name]}
                      target="_blank"
                      rel="noreferrer"
                      className="text-ink hover:text-brand-600 font-medium"
                    >
                      {NAMES[s.name] ?? s.name}
                    </a>
                  ) : (
                    <span className="text-ink font-medium">
                      {NAMES[s.name] ?? s.name}
                    </span>
                  )}
                  {HOMEPAGES[s.name] && (
                    <div className="text-ink-faint text-xs">
                      {HOMEPAGES[s.name].replace(/^https?:\/\/(www\.)?/, '')}
                    </div>
                  )}
                </Td>
                <Td className="text-ink-body">
                  {s.paid ? 'Paid feed' : 'Free tier'}
                </Td>
                <Td className="text-ink-body">
                  {s.backfill_available ? (
                    'available'
                  ) : (
                    <span className="text-ink-faint">—</span>
                  )}
                </Td>
              </TR>
            ))}
          </TBody>
        </Table>
      </div>
    </Panel>
  );
}
