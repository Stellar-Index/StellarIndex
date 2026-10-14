'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { Skeleton, TBody, TR, Table, Td, Th, THead } from '@/components/ui';
import { PANEL_LABEL_CLASS } from '@/components/ui/Page';
import { apiGet, asExample } from '@/api/client';
import {
  compareDecimalStrings,
  formatCompact,
  formatCompactUnits,
} from '@/lib/format';
import { DexTvlHeadline } from '@/app/protocols/ProtocolTvlPanel';
import { protocolMeta } from '@/app/protocols/registry';
import { useProtocolTvls, type ProtocolTvl } from './useProtocolTvls';
import { DexVolumeShare } from './DexVolumeShare';
import { hrefFor } from '@/lib/hrefFor';

interface SourceRow {
  name: string;
  class: string;
  subclass: string;
  trade_count_24h?: number;
  markets_count_24h?: number;
  volume_24h_usd?: string | null;
}

/**
 * DexProtocolsTable — per-DEX summary row (volume, trades, markets).
 * Backed by /v1/sources?include=stats filtered to Subclass=DEX.
 * Sits above the all-pools table on /dexes.
 */
export function DexProtocolsTable() {
  const q = useQuery<SourceRow[]>({
    queryKey: ['/v1/sources', 'stats', 'dex'],
    queryFn: async () => {
      const env = await apiGet<{ data: SourceRow[] }>('/v1/sources', {
        include: 'stats',
      });
      const arr = env.data ?? [];
      return arr
        .filter((s) => s.class === 'exchange' && s.subclass === 'dex')
        .sort((a, b) => {
          const c = compareDecimalStrings(
            b.volume_24h_usd || '0',
            a.volume_24h_usd || '0',
          );
          if (c) return c;
          return (b.trade_count_24h ?? 0) - (a.trade_count_24h ?? 0);
        });
    },
    refetchInterval: 60_000,
  });

  const rows = q.data ?? [];
  // Absent (the /v1/sources fetch failed) ≠ empty (no DEX registered).
  // Without this the error path falls into the empty state and claims
  // Stellar has no active DEXes.
  const registryAvailable = q.data != null;
  // `?include=stats` soft-fails server-side and every stats column is
  // omitempty — on that degrade EVERY row arrives bare. Detect wholesale
  // absence so cells render '—' rather than a fabricated "0 trades /
  // 0 pools" claim; with the join present, a bare field is a genuine
  // present-and-zero (omitempty drops 0).
  const statsAvailable = rows.some(
    (r) =>
      r.volume_24h_usd != null ||
      r.trade_count_24h != null ||
      r.markets_count_24h != null,
  );
  const tvlQuery = useProtocolTvls();
  const tvls = tvlQuery.data?.byProtocol ?? {};
  const tvlTotal = tvlQuery.data?.total;

  // The headline is the exact sum of the TVL column beneath it, so it
  // may only appear when every protocol it sums is a visible row that
  // actually shows a figure. This table is built from /v1/sources
  // (DEX subclass), a DIFFERENT registry from the /v1/protocols rows
  // the total is summed over — so a summed protocol can be missing
  // here even when the server admitted it. A headline that does not
  // reconcile with the column below is a bare number again, which is
  // the one thing this surface exists not to serve.
  const charted = new Set(
    rows.filter((r) => tvls[r.name] != null).map((r) => r.name),
  );
  const headline =
    tvlTotal != null &&
    tvlTotal.protocols.length > 0 &&
    tvlTotal.protocols.every((p) => charted.has(p))
      ? tvlTotal
      : null;

  return (
    <Panel
      headingLevel={2}
      title="DEX protocols"
      hint="Per-protocol 24h activity"
      source={asExample('/v1/sources', { include: 'stats' })}
      bodyClassName="-mx-4 -mb-4"
    >
      {headline && (
        <div className="mx-4 mb-5">
          <DexTvlHeadline total={headline} />
        </div>
      )}
      {statsAvailable && (
        <div className="mx-4 mb-4">
          <div className={`${PANEL_LABEL_CLASS} mb-2`}>24h volume share</div>
          <DexVolumeShare rows={rows} />
        </div>
      )}
      <div className="overflow-x-auto">
        <Table>
          <THead>
            <tr>
              <Th>Protocol</Th>
              <Th align="right">TVL</Th>
              <Th align="right">24h volume</Th>
              <Th align="right">24h trades</Th>
              <Th align="right">Active pools</Th>
            </tr>
          </THead>
          <TBody>
            {q.isLoading && (
              <TR>
                <Td colSpan={5}>
                  <Skeleton className="h-5 w-48" />
                </Td>
              </TR>
            )}
            {!q.isLoading && !registryAvailable && (
              <TR>
                <Td colSpan={5} className="text-ink-muted">
                  Protocol list unavailable right now.{' '}
                  <Link
                    href="/status"
                    className="text-brand-600 hover:underline"
                  >
                    Status
                  </Link>
                </Td>
              </TR>
            )}
            {!q.isLoading && registryAvailable && rows.length === 0 && (
              <TR>
                <Td colSpan={5} className="text-ink-muted">
                  No DEX protocols reporting 24h activity.
                </Td>
              </TR>
            )}
            {rows.map((r) => {
              const vol = r.volume_24h_usd;
              return (
                <TR key={r.name}>
                  <Td>
                    <Link
                      href={hrefFor.dex(r.name)}
                      className="text-ink hover:text-brand-600 font-medium"
                    >
                      {protocolMeta(r.name)?.label ?? r.name}
                    </Link>
                  </Td>
                  <Td align="right">
                    <TvlCell tvl={tvls[r.name]} />
                  </Td>
                  <Td align="right" className="font-mono tabular-nums">
                    {vol && compareDecimalStrings(vol, '0') === 1 ? (
                      `$${formatCompactUnits(vol)}`
                    ) : (
                      <span className="text-ink-faint">—</span>
                    )}
                  </Td>
                  <Td align="right" className="font-mono tabular-nums">
                    {countCell(r.trade_count_24h, statsAvailable)}
                  </Td>
                  <Td align="right" className="font-mono tabular-nums">
                    {countCell(r.markets_count_24h, statsAvailable)}
                  </Td>
                </TR>
              );
            })}
          </TBody>
        </Table>
      </div>
    </Panel>
  );
}

// A bare field beside present stats is a real zero; with the stats join
// missing wholesale it is unknown, never "0".
function countCell(n: number | undefined, statsAvailable: boolean): string {
  if (n && n > 0) return formatCompact(n);
  return statsAvailable ? '0' : '—';
}

/**
 * TvlCell — TVL from the /v1/protocols snapshot. Protocols without an
 * absolute reserve source have no snapshot entry and render "—" (an
 * honest absence, never a fabricated zero); a snapshot with unpriced
 * pools is a lower bound, flagged with a "≥" prefix and the basis as
 * the hover title.
 */
function TvlCell({ tvl }: { tvl?: ProtocolTvl }) {
  if (!tvl) return <span className="text-ink-faint">—</span>;
  if (compareDecimalStrings(tvl.tvl_usd, '0') !== 1)
    return <span className="text-ink-faint">—</span>;
  const lowerBound = tvl.unpriced_pools > 0;
  return (
    <span className="font-mono tabular-nums" title={tvl.basis}>
      {lowerBound && (
        <span className="text-ink-muted font-sans" aria-hidden>
          ≥{' '}
        </span>
      )}
      ${formatCompactUnits(tvl.tvl_usd)}
    </span>
  );
}
