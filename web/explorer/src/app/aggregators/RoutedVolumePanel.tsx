'use client';

import Link from 'next/link';
import { hrefFor } from '@/lib/hrefFor';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import type { components } from '@/api/types';
import { formatReadable, formatRelative } from '@/lib/format';
import { RoutedVolumeBars } from './RoutedVolumeBars';

type AggregatorRow = components['schemas']['AggregatorRow'];

function fmtVolume(v: string | null): string {
  if (v === null) return '—';
  return formatReadable(v, true) ?? v;
}

/**
 * RoutedVolumePanel — live routed-via attribution from
 * /v1/aggregators: how many trades (and how much USD volume)
 * reached the underlying DEX pairs via each registered router over
 * the trailing 24 h. EVERY registry row renders — filtering to
 * kind === 'router' would silently drop the
 * aggregator-vault entries the endpoint returns. Vault-kind entries never accrue per-tx routing tags
 * (their capital state lives on the protocol pages), so their
 * routed columns render "n/a" — not a zero-claim.
 */
export function RoutedVolumePanel() {
  const q = useQuery<AggregatorRow[]>({
    queryKey: ['/v1/aggregators'],
    queryFn: async () => {
      const env = await apiGet<{ data: AggregatorRow[] }>('/v1/aggregators');
      return env.data ?? [];
    },
    refetchInterval: 60_000,
  });

  // Routers first (they carry the live numbers), then vaults.
  const rows = [...(q.data ?? [])].sort((a, b) =>
    a.kind === b.kind
      ? a.name.localeCompare(b.name)
      : a.kind === 'router'
        ? -1
        : 1,
  );

  return (
    <Panel
      headingLevel={2}
      title="Routed volume (24h)"
      hint="Trades tagged routed_via — same-tx attribution of router invocations to underlying pair trades; vault entries are listed for completeness (they don't accrue per-tx tags)"
      source={asExample('/v1/aggregators')}
      bodyClassName="-mx-4"
    >
      {!q.isError && (
        <div className="px-4 pb-4">
          <RoutedVolumeBars rows={rows} />
        </div>
      )}
      <div className="overflow-x-auto">
        <table className="divide-line min-w-full divide-y text-sm">
          <thead>
            <tr className="text-ink-muted text-left text-[10px] tracking-wider uppercase">
              <Th>Aggregator</Th>
              <Th>Kind</Th>
              <Th>Protocol</Th>
              <Th align="right">Routed trades</Th>
              <Th align="right">Routed volume</Th>
              <Th align="right">Last routed</Th>
            </tr>
          </thead>
          <tbody className="divide-line-subtle divide-y">
            {q.isLoading && (
              <tr>
                <td
                  colSpan={6}
                  className="text-ink-muted px-4 py-6 text-center text-sm"
                >
                  Loading attribution…
                </td>
              </tr>
            )}
            {q.isError && (
              <tr>
                <td
                  colSpan={6}
                  className="text-ink-muted px-4 py-6 text-center text-sm"
                >
                  Attribution rollup unavailable.
                </td>
              </tr>
            )}
            {!q.isLoading && !q.isError && rows.length === 0 && (
              <tr>
                <td
                  colSpan={6}
                  className="text-ink-muted px-4 py-6 text-center text-sm"
                >
                  No aggregators registered.
                </td>
              </tr>
            )}
            {rows.map((r) => (
              <tr key={r.contract_id} className="hover:bg-surface-muted">
                <Td>
                  <Link
                    href={`/contracts/${encodeURIComponent(r.contract_id)}/`}
                    className="text-ink hover:text-brand-600 font-medium"
                    title={r.contract_id}
                  >
                    {r.name}
                  </Link>
                  {r.auto_discovered && (
                    <span
                      className="border-line text-ink-muted ml-1.5 rounded border px-1 py-0.5 text-[9px] tracking-wider uppercase"
                      title="Evidence-observed, not vendor- or WASM-audit-verified"
                    >
                      unverified
                    </span>
                  )}
                  {r.notes && r.notes.length > 0 && (
                    <span
                      className="text-ink-muted ml-1 cursor-help"
                      title={r.notes.join(' ')}
                      aria-label="Coverage caveat"
                    >
                      *
                    </span>
                  )}
                </Td>
                <Td>
                  <span className="bg-surface-subtle text-ink-body rounded-sm px-1.5 py-0.5 font-mono text-[10px] tracking-wider uppercase">
                    {r.kind}
                  </span>
                </Td>
                <Td>
                  <Link
                    href={hrefFor.protocol(r.protocol)}
                    className="text-brand-600 hover:underline"
                  >
                    {r.protocol}
                  </Link>
                </Td>
                {r.kind === 'router' ? (
                  <>
                    <Td align="right">
                      <span className="font-mono text-xs">
                        {r.routed_trades_24h.toLocaleString('en-US')}
                      </span>
                    </Td>
                    <Td align="right">
                      <span className="font-mono text-xs">
                        {fmtVolume(r.routed_volume_24h_usd)}
                      </span>
                    </Td>
                    <Td align="right">
                      <span className="text-ink-muted text-xs">
                        {r.last_routed_at
                          ? formatRelative(r.last_routed_at)
                          : 'never'}
                      </span>
                    </Td>
                  </>
                ) : (
                  // Vault kinds don't route per-tx trades — "n/a" is the
                  // honest cell, not the endpoint's placeholder zeros.
                  <Td align="right" colSpan={3}>
                    <span
                      className="text-ink-faint text-xs"
                      title="Vault-kind entries hold + deploy capital; they never accrue per-tx routed_via tags. See the protocol page for their capital state."
                    >
                      n/a — holds capital, doesn&apos;t route trades
                    </span>
                  </Td>
                )}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <p className="border-line text-ink-muted border-t px-4 py-2 text-xs">
        <span title="Each router invocation is joined to the swap trades in the same transaction (first-wins, never re-tagged); a wrapping aggregator gets its own row when registered.">
          Routed volume, USD
        </span>
        ; &ldquo;—&rdquo; means not yet valued, not zero. Rows marked{' '}
        <span className="border-line rounded border px-1 py-0.5 text-[9px] tracking-wider uppercase">
          unverified
        </span>{' '}
        or with <span title="hover any * for details">*</span> carry coverage
        caveats — hover for detail.
      </p>
    </Panel>
  );
}

function Th({
  children,
  align,
}: {
  children: React.ReactNode;
  align?: 'left' | 'right';
}) {
  return (
    <th
      scope="col"
      className={`px-4 py-2 ${align === 'right' ? 'text-right' : 'text-left'}`}
    >
      {children}
    </th>
  );
}

function Td({
  children,
  align,
  colSpan,
}: {
  children: React.ReactNode;
  align?: 'left' | 'right';
  colSpan?: number;
}) {
  return (
    <td
      colSpan={colSpan}
      className={`px-4 py-2 ${align === 'right' ? 'text-right' : 'text-left'}`}
    >
      {children}
    </td>
  );
}
