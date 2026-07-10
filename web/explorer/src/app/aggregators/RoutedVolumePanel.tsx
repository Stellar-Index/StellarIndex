'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import type { components } from '@/api/types';

type AggregatorRow = components['schemas']['AggregatorRow'];

const usdFmt = new Intl.NumberFormat('en-US', {
  style: 'currency',
  currency: 'USD',
  maximumFractionDigits: 0,
});

function fmtVolume(v: string | null): string {
  if (v === null) return '—';
  const n = Number(v);
  if (!Number.isFinite(n)) return v;
  return usdFmt.format(n);
}

function fmtAgo(iso: string | null): string {
  if (!iso) return 'never';
  const ms = Date.now() - new Date(iso).getTime();
  if (ms < 0) return 'just now';
  const mins = Math.floor(ms / 60_000);
  if (mins < 1) return 'just now';
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 48) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}

/**
 * RoutedVolumePanel — live routed-via attribution from
 * /v1/aggregators: how many trades (and how much USD volume)
 * reached the underlying DEX pairs via each registered router over
 * the trailing 24 h. Vault-kind entries never accrue per-tx tags
 * (their capital state lives on the protocol pages), so this table
 * shows routers only.
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

  const routers = (q.data ?? []).filter((r) => r.kind === 'router');

  return (
    <Panel
      title="Routed volume (24h)"
      hint="Trades tagged routed_via — same-tx attribution of router invocations to underlying pair trades"
      source={asExample('/v1/aggregators')}
      bodyClassName="-mx-4"
    >
      <div className="overflow-x-auto">
        <table className="min-w-full divide-y divide-line text-sm">
          <thead>
            <tr className="text-left text-[10px] uppercase tracking-wider text-ink-muted">
              <Th>Router</Th>
              <Th>Protocol</Th>
              <Th align="right">Routed trades</Th>
              <Th align="right">Routed volume</Th>
              <Th align="right">Last routed</Th>
            </tr>
          </thead>
          <tbody className="divide-y divide-line-subtle">
            {q.isLoading && (
              <tr>
                <td colSpan={5} className="px-4 py-6 text-center text-sm text-ink-muted">
                  Loading attribution…
                </td>
              </tr>
            )}
            {q.isError && (
              <tr>
                <td colSpan={5} className="px-4 py-6 text-center text-sm text-ink-muted">
                  Attribution rollup unavailable.
                </td>
              </tr>
            )}
            {!q.isLoading && !q.isError && routers.length === 0 && (
              <tr>
                <td colSpan={5} className="px-4 py-6 text-center text-sm text-ink-muted">
                  No routers registered.
                </td>
              </tr>
            )}
            {routers.map((r) => (
              <tr key={r.contract_id} className="hover:bg-surface-muted">
                <Td>
                  <Link
                    href={`/contracts/${encodeURIComponent(r.contract_id)}/`}
                    className="font-medium text-ink hover:text-brand-600"
                    title={r.contract_id}
                  >
                    {r.name}
                  </Link>
                  {r.auto_discovered && (
                    <span
                      className="ml-1.5 rounded border border-line px-1 py-0.5 text-[9px] uppercase tracking-wider text-ink-muted"
                      title="Evidence-observed, not vendor- or WASM-audit-verified"
                    >
                      unverified
                    </span>
                  )}
                  {r.notes && r.notes.length > 0 && (
                    <span
                      className="ml-1 cursor-help text-ink-muted"
                      title={r.notes.join(' ')}
                      aria-label="Coverage caveat"
                    >
                      *
                    </span>
                  )}
                </Td>
                <Td>
                  <Link
                    href={`/protocols/${r.protocol}`}
                    className="text-brand-600 hover:underline"
                  >
                    {r.protocol}
                  </Link>
                </Td>
                <Td align="right">
                  <span className="font-mono text-xs">{r.routed_trades_24h.toLocaleString()}</span>
                </Td>
                <Td align="right">
                  <span className="font-mono text-xs">{fmtVolume(r.routed_volume_24h_usd)}</span>
                </Td>
                <Td align="right">
                  <span className="text-xs text-ink-muted">{fmtAgo(r.last_routed_at)}</span>
                </Td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <p className="border-t border-line px-4 py-2 text-xs text-ink-muted">
        Attribution joins each router invocation to the pair-level swap
        trades in the same transaction (first-wins, never re-tagged).
        Volume is the USD valuation of the routed trades; &ldquo;—&rdquo;
        means the window&apos;s routed trades haven&apos;t received a USD
        valuation yet, not zero. A sub-invocation call (an aggregator
        wrapping the router) is attributed to that aggregator&apos;s own
        row when it&apos;s a registered contract; rows marked{' '}
        <span className="rounded border border-line px-1 py-0.5 text-[9px] uppercase tracking-wider">
          unverified
        </span>{' '}
        or with <span title="hover any * for details">*</span> carry
        coverage caveats — hover for detail.
      </p>
    </Panel>
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
