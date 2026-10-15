'use client';

import Link from 'next/link';
import { hrefFor } from '@/lib/hrefFor';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import type { components } from '@/api/types';
import { formatReadable, formatRelative } from '@/lib/format';
import { protocolMeta } from '@/app/protocols/registry';
import {
  Badge,
  Skeleton,
  TBody,
  TR,
  Table,
  Td,
  Th,
  THead,
  LinkRow,
} from '@/components/ui';
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
      hint="Trades tagged routed_via: same-transaction attribution of router invocations to underlying pair trades. Vaults are listed for completeness; they never accrue per-transaction tags."
      source={asExample('/v1/aggregators')}
      bodyClassName="-mx-4 -mb-4"
    >
      {!q.isError && rows.length > 0 && (
        <div className="px-4 pb-4">
          <RoutedVolumeBars rows={rows} />
        </div>
      )}
      <div className="overflow-x-auto">
        <Table>
          <THead>
            <tr>
              <Th>Aggregator</Th>
              <Th>Kind</Th>
              <Th>Protocol</Th>
              <Th align="right">Routed trades</Th>
              <Th align="right">Routed volume</Th>
              <Th align="right">Last routed</Th>
            </tr>
          </THead>
          <TBody>
            {q.isLoading && (
              <tr>
                <td colSpan={6} className="px-4 py-3">
                  <Skeleton className="h-5 w-full" />
                </td>
              </tr>
            )}
            {q.isError && (
              <tr>
                <td
                  colSpan={6}
                  className="text-ink-muted px-4 py-6 text-center text-sm"
                >
                  Aggregator registry unavailable right now.{' '}
                  <Link
                    href="/status"
                    className="text-brand-600 hover:underline"
                  >
                    Status
                  </Link>
                </td>
              </tr>
            )}
            {q.data && rows.length === 0 && (
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
              <LinkRow
                href={`/contracts/${encodeURIComponent(r.contract_id)}/`}
                key={r.contract_id}
              >
                <Td>
                  <div className="flex flex-wrap items-center gap-2">
                    <Link
                      href={`/contracts/${r.contract_id}/`}
                      className="text-ink hover:text-brand-600 font-medium"
                    >
                      {r.name}
                    </Link>
                    {r.auto_discovered && (
                      <Badge
                        tone="warn"
                        title="Discovered from on-chain calls; not yet hand-verified"
                      >
                        unverified
                      </Badge>
                    )}
                  </div>
                  {r.notes && r.notes.length > 0 && (
                    <div
                      className="text-ink-faint mt-0.5 max-w-xs truncate text-xs"
                      title={r.notes.join(' · ')}
                    >
                      {r.notes.join(' · ')}
                    </div>
                  )}
                </Td>
                <Td className="text-ink-muted">
                  {KIND_LABEL[r.kind] ?? r.kind}
                </Td>
                <Td>
                  {r.protocol ? (
                    <Link
                      href={hrefFor.protocol(r.protocol)}
                      className="text-ink hover:text-brand-600"
                    >
                      {protocolMeta(r.protocol)?.label ?? r.protocol}
                    </Link>
                  ) : (
                    <span className="text-ink-faint">—</span>
                  )}
                </Td>
                {r.kind === 'router' ? (
                  <>
                    <Td align="right" className="font-mono">
                      {r.routed_trades_24h == null
                        ? '—'
                        : r.routed_trades_24h.toLocaleString('en-US')}
                    </Td>
                    <Td align="right" className="font-mono">
                      {fmtVolume(r.routed_volume_24h_usd)}
                    </Td>
                    <Td align="right" className="text-ink-muted">
                      {r.last_routed_at
                        ? formatRelative(r.last_routed_at)
                        : 'never'}
                    </Td>
                  </>
                ) : (
                  <Td align="right" colSpan={3} className="text-ink-faint">
                    n/a — holds capital, does not route trades
                  </Td>
                )}
              </LinkRow>
            ))}
          </TBody>
        </Table>
      </div>
      <p className="text-ink-muted border-line border-t px-4 py-3 text-xs">
        Routed volume is a subset of the pair volume on the DEX pages, not
        volume in addition to it.
      </p>
    </Panel>
  );
}

const KIND_LABEL: Record<string, string> = {
  router: 'Router',
  vault: 'Yield vault',
  'aggregator-vault': 'Yield vault',
};
