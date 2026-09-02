'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { formatCompact } from '@/lib/format';
import { sourceToneClass } from '@/lib/pillTone';
import { SourceSparkline } from '@/components/SourceSparkline';
import { useProtocolTvls, type ProtocolTvl } from './useProtocolTvls';

interface VolumeBucket {
  hour: string;
  volume_usd: string;
}

interface SourceRow {
  name: string;
  class: string;
  subclass: string;
  trade_count_24h?: number;
  markets_count_24h?: number;
  volume_24h_usd?: string | null;
  volume_history_24h?: VolumeBucket[];
}

/**
 * DexProtocolsTable — per-DEX summary row (volume, trades, markets).
 * Backed by /v1/sources?include=stats filtered to Subclass=DEX.
 * Sits above the all-pools table on /dexes.
 */
export function DexProtocolsTable() {
  const q = useQuery<SourceRow[]>({
    queryKey: ['/v1/sources', 'stats,sparkline', 'dex'],
    queryFn: async () => {
      const env = await apiGet<{ data: SourceRow[] }>('/v1/sources', { include: 'stats,sparkline' });
      const arr = env.data ?? [];
      return arr
        .filter((s) => s.class === 'exchange' && s.subclass === 'dex')
        .sort((a, b) => {
          const av = a.volume_24h_usd ? Number(a.volume_24h_usd) : 0;
          const bv = b.volume_24h_usd ? Number(b.volume_24h_usd) : 0;
          if (bv !== av) return bv - av;
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
  const tvls = useProtocolTvls().data ?? {};

  return (
    <Panel
      headingLevel={2}
      title="DEX protocols"
      hint="Per-protocol 24h activity"
      source={asExample('/v1/sources', { include: 'stats' })}
      bodyClassName="-mx-4"
    >
      <div className="overflow-x-auto">
        <table className="min-w-full divide-y divide-line text-sm">
          <thead>
            <tr className="text-left text-[10px] uppercase tracking-wider text-ink-muted">
              <Th>Protocol</Th>
              <Th align="right">TVL</Th>
              <Th align="right">24h volume</Th>
              <Th>24h chart</Th>
              <Th align="right">24h trades</Th>
              <Th align="right">Active pools</Th>
              <Th align="right">VWAP weight</Th>
            </tr>
          </thead>
          <tbody className="divide-y divide-line-subtle">
            {q.isLoading && (
              <tr>
                <td colSpan={7} className="px-4 py-6 text-center text-sm text-ink-muted">
                  Loading protocols…
                </td>
              </tr>
            )}
            {!q.isLoading && !registryAvailable && (
              <tr>
                <td colSpan={7} className="px-4 py-6 text-center text-sm text-ink-muted">
                  Protocol list unavailable right now — retry shortly.
                </td>
              </tr>
            )}
            {!q.isLoading && registryAvailable && rows.length === 0 && (
              <tr>
                <td colSpan={7} className="px-4 py-6 text-center text-sm text-ink-muted">
                  No DEX protocols reporting 24h activity.
                </td>
              </tr>
            )}
            {rows.map((r) => {
              const vol = r.volume_24h_usd ? Number(r.volume_24h_usd) : null;
              const tone = sourceToneClass(r.name);
              return (
                <tr key={r.name} className="hover:bg-surface-muted">
                  <Td>
                    <Link
                      href={`/dexes/${r.name}`}
                      className={`inline-block rounded-sm px-1.5 py-0.5 text-[11px] font-medium uppercase tracking-wider hover:underline ${tone}`}
                    >
                      {r.name}
                    </Link>
                  </Td>
                  <Td align="right">
                    <TvlCell tvl={tvls[r.name]} />
                  </Td>
                  <Td align="right">
                    {vol != null && Number.isFinite(vol) && vol > 0 ? (
                      <span className="font-mono tabular-nums">${formatCompact(vol)}</span>
                    ) : (
                      <span className="text-ink-faint">—</span>
                    )}
                  </Td>
                  <Td>
                    <SourceSparkline buckets={r.volume_history_24h} />
                  </Td>
                  <Td align="right">
                    <span className="font-mono tabular-nums text-ink-body">
                      {r.trade_count_24h && r.trade_count_24h > 0
                        ? formatCompact(r.trade_count_24h)
                        : statsAvailable
                          ? '0'
                          : '—'}
                    </span>
                  </Td>
                  <Td align="right">
                    <span className="font-mono tabular-nums text-ink-body">
                      {r.markets_count_24h && r.markets_count_24h > 0
                        ? formatCompact(r.markets_count_24h)
                        : statsAvailable
                          ? '0'
                          : '—'}
                    </span>
                  </Td>
                  <Td align="right">
                    <Link
                      href={`/dexes/${r.name}`}
                      className="text-xs text-brand-600 hover:underline"
                    >
                      details →
                    </Link>
                  </Td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </Panel>
  );
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
  const v = Number(tvl.tvl_usd);
  if (!Number.isFinite(v) || v <= 0) return <span className="text-ink-faint">—</span>;
  const lowerBound = tvl.unpriced_pools > 0;
  return (
    <span className="font-mono tabular-nums" title={tvl.basis}>
      {lowerBound ? '≥ ' : ''}${formatCompact(v)}
    </span>
  );
}

function Th({ children, align }: { children: React.ReactNode; align?: 'left' | 'right' }) {
  return (
    <th
      scope="col"
      className={`px-4 py-2 ${align === 'right' ? 'text-right' : 'text-left'}`}
    >
      {children}
    </th>
  );
}

function Td({ children, align }: { children: React.ReactNode; align?: 'left' | 'right' }) {
  return (
    <td className={`px-4 py-2 ${align === 'right' ? 'text-right' : 'text-left'}`}>{children}</td>
  );
}
