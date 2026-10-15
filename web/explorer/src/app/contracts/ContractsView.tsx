'use client';

import { useState, type ReactNode } from 'react';
import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { useSACWrappers } from '@/api/hooks';
import { formatCompact, truncateMiddle } from '@/lib/format';
import { isNativeXlmSac } from '@/lib/asset-label';
import { useTableSort, SortableTh, type SortColumn } from '@/lib/useTableSort';
import { StackedColumns } from '@/components/charts/Bars';
import {
  InlineBar,
  ProtocolMixDonut,
  RegistryCharts,
} from '@/components/ContractCharts';
import {
  Badge,
  Button,
  Container,
  PageHeader,
  Segmented,
  Skeleton,
  Stat,
  StatCell,
  StatGrid,
  TBody,
  TR,
  Table,
  Td,
  Th,
  THead,
  LinkRow,
} from '@/components/ui';
import { categoryLabel, protocolMeta } from '../protocols/registry';
import { formatTimestamp } from '../explorer-shared';
import type { paths } from '@/api/types';

// GET /v1/contracts response body, derived from the generated OpenAPI
// contract (src/api/types.ts, `make web-generate-api`).
type DirectoryResp = NonNullable<
  paths['/contracts']['get']['responses'][200]['content']['application/json']['data']
>;

// GET /v1/contracts/stats response body.
type StatsResp = NonNullable<
  paths['/contracts/stats']['get']['responses'][200]['content']['application/json']['data']
>;

// Mirrors internal/api/v1/protocols.go ProtocolView — the slice the registry
// view needs: which contracts each protocol's factory owns, and how many.
interface RegistryProtocol {
  name: string;
  category: string;
  factories: string[];
  contract_count: number;
  events_24h: number;
}

type SACMap = Record<string, string> | undefined;

/**
 * ContractsView — the contracts explorer, two views over one hinge (the
 * factory-anchored attribution registry, ADR-0035):
 *
 *   - "Most active" — the raw event-ranked directory (GET /v1/contracts).
 *     Dominated by SACs and high-traffic system contracts, so its type
 *     column is mostly the on-chain kind: raw activity and protocol
 *     attribution are orthogonal.
 *   - "Registry" — the contracts the protocol registry attributes
 *     (GET /v1/protocols), grouped by protocol, each factory linked into
 *     its hub.
 */
export function ContractsView() {
  const { data: sacMap } = useSACWrappers();
  const [view, setView] = useState<'active' | 'registry'>('active');

  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[{ label: 'Home', href: '/' }, { label: 'Contracts' }]}
        title="Contracts"
        actions={
          <Segmented
            ariaLabel="Contracts view"
            value={view}
            onChange={(v) => setView(v as 'active' | 'registry')}
            options={[
              { label: 'Most active', value: 'active' },
              { label: 'Registry', value: 'registry' },
            ]}
          />
        }
      />
      <DeploymentStats />
      {view === 'active' ? (
        <MostActivePanel sacMap={sacMap} />
      ) : (
        <RegistryPanel />
      )}
    </Container>
  );
}

/** Type label for a directory row: protocol attribution wins over the on-chain kind. */
export function contractTypeBadge(c: {
  protocol?: string | null;
  type?: string | null;
}) {
  if (c.protocol) {
    return (
      <Link
        href={`/protocols/${encodeURIComponent(c.protocol)}`}
        className="inline-flex rounded-full hover:opacity-80"
      >
        <Badge tone="brand">
          Protocol · {protocolMeta(c.protocol)?.label ?? c.protocol}
        </Badge>
      </Link>
    );
  }
  if (c.type === 'sac') return <Badge tone="neutral">SAC</Badge>;
  if (c.type === 'wasm') return <Badge tone="neutral">Contract</Badge>;
  return <span className="text-ink-faint">—</span>;
}

export function DeploymentStats() {
  const { data } = useQuery<StatsResp>({
    queryKey: ['/v1/contracts/stats'],
    staleTime: 300_000,
    retry: false,
    queryFn: async () => {
      const env = await apiGet<{ data: StatsResp }>('/v1/contracts/stats');
      return env.data;
    },
  });
  if (!data) return null;
  const prefix = data.lower_bound ? '≥ ' : '';
  const n = (v: number) => `${prefix}${formatCompact(v)}`;
  return (
    <div className="space-y-6">
      <StatGrid cols={4} className="grid-cols-2">
        <StatCell>
          <Stat label="Total deployed" value={n(data.total_deployed)} />
        </StatCell>
        <StatCell>
          <Stat label="SAC" value={n(data.total_sac)} />
        </StatCell>
        <StatCell>
          <Stat label="WASM" value={n(data.total_wasm)} />
        </StatCell>
        <StatCell>
          <Stat
            label="Active (90d)"
            value={
              data.active_90d == null ? '—' : formatCompact(data.active_90d)
            }
          />
        </StatCell>
      </StatGrid>
      <Panel
        headingLevel={2}
        title="Deployments"
        hint="New contracts per month, SAC and WASM"
        source={asExample('/v1/contracts/stats')}
      >
        <StackedColumns
          ariaLabel="Contracts deployed per month"
          series={[
            { label: 'SAC', color: 'var(--color-brand-500)' },
            { label: 'WASM', color: 'var(--color-up)' },
          ]}
          buckets={data.deployments.map((d) => ({
            label: d.month ?? '',
            values: [d.sac ?? 0, d.wasm ?? 0],
          }))}
        />
      </Panel>
    </div>
  );
}

function NoteRow({
  colSpan,
  children,
}: {
  colSpan: number;
  children: ReactNode;
}) {
  return (
    <TR className="hover:bg-transparent">
      <Td colSpan={colSpan} className="text-ink-muted py-4">
        {children}
      </Td>
    </TR>
  );
}

function MostActivePanel({ sacMap }: { sacMap: SACMap }) {
  const { data, isLoading, isError, refetch } = useQuery<DirectoryResp>({
    queryKey: ['/v1/contracts', 30],
    staleTime: 60_000,
    retry: false,
    queryFn: async () => {
      const env = await apiGet<{ data: DirectoryResp }>('/v1/contracts', {
        days: 30,
        limit: 100,
      });
      return env.data;
    },
  });
  const rows = data?.contracts ?? [];

  // Sortable columns; the default keeps the API's events-ranked order
  // until a header is clicked.
  const contractSortColumns: SortColumn<(typeof rows)[number], string>[] = [
    { key: 'protocol', value: (c) => c.protocol ?? '', initialDir: 'asc' },
    { key: 'events', value: (c) => c.events },
    { key: 'last_seen', value: (c) => c.last_seen ?? '' },
  ];
  const { sorted, sort, toggle, ariaSort } = useTableSort<
    (typeof rows)[number],
    string
  >(rows, contractSortColumns, null);
  const maxEvents = Math.max(0, ...rows.map((c) => c.events ?? 0));

  return (
    <Panel
      headingLevel={2}
      title={
        rows.length > 0
          ? `Most active (${formatCompact(rows.length)})`
          : 'Most active'
      }
      hint={
        <span title="Leaders are SACs and high-traffic system contracts. Attribution lives in the Registry view, so most rows carry no protocol tag by design.">
          ranked by raw event volume · 30d
        </span>
      }
      source={asExample('/v1/contracts', { days: 30, limit: 100 })}
      bodyClassName="-mx-4 -mb-4"
    >
      {rows.length > 0 && (
        <div className="px-4 pb-4">
          <ProtocolMixDonut
            rows={rows.map((c) => ({
              ...c,
              protocol: c.protocol
                ? (protocolMeta(c.protocol)?.label ?? c.protocol)
                : c.protocol,
            }))}
          />
        </div>
      )}
      <div className="overflow-x-auto">
        <Table>
          <THead>
            <tr>
              <Th>Contract</Th>
              <SortableTh
                label="Type"
                sortKey="protocol"
                sort={sort}
                onSort={toggle}
                ariaSort={ariaSort}
                className="hidden sm:table-cell"
              />
              <SortableTh
                label="Events (30d)"
                sortKey="events"
                sort={sort}
                onSort={toggle}
                ariaSort={ariaSort}
                align="right"
              />
              <SortableTh
                label="Last seen"
                sortKey="last_seen"
                sort={sort}
                onSort={toggle}
                ariaSort={ariaSort}
                align="right"
                className="hidden sm:table-cell"
              />
            </tr>
          </THead>
          <TBody>
            {isError && (
              <NoteRow colSpan={4}>
                Contract list unavailable — the directory read did not return.
                <Button
                  variant="secondary"
                  size="sm"
                  className="ml-3"
                  onClick={() => refetch()}
                >
                  Retry
                </Button>
              </NoteRow>
            )}
            {!isError && isLoading && (
              <TR className="hover:bg-transparent">
                <Td colSpan={4}>
                  <Skeleton className="h-4 w-64" />
                </Td>
              </TR>
            )}
            {!isError && !isLoading && rows.length === 0 && (
              <NoteRow colSpan={4}>
                No contract activity in the last 30 days.
              </NoteRow>
            )}
            {sorted.map((c) => {
              const id = c.contract_id ?? '';
              const wrapped = isNativeXlmSac(id) ? 'native' : sacMap?.[id];
              const code =
                wrapped == null
                  ? null
                  : wrapped === 'native'
                    ? 'XLM'
                    : wrapped.split(/[:-]/)[0];
              return (
                <LinkRow
                  href={`/contracts/${encodeURIComponent(id)}/`}
                  key={id}
                >
                  <Td>
                    <Link
                      href={`/contracts/${encodeURIComponent(id)}/`}
                      className="text-brand-600 font-mono text-xs whitespace-nowrap hover:underline"
                      title={id}
                    >
                      {truncateMiddle(id, 8, 6)}
                    </Link>
                    {code && (
                      <span className="text-ink-muted ml-2 text-xs whitespace-nowrap">
                        {code} SAC
                      </span>
                    )}
                  </Td>
                  <Td className="hidden sm:table-cell">
                    {contractTypeBadge(c)}
                  </Td>
                  <Td align="right" className="font-mono">
                    <span className="flex items-center justify-end gap-3 whitespace-nowrap">
                      <span className="hidden sm:contents">
                        <InlineBar
                          value={c.events ?? 0}
                          max={maxEvents}
                          label={`${formatCompact(c.events ?? 0)} events`}
                        />
                      </span>
                      <span className="inline-block w-14 text-right">
                        {formatCompact(c.events ?? 0)}
                      </span>
                    </span>
                  </Td>
                  <Td
                    align="right"
                    className="text-ink-muted hidden font-mono text-xs sm:table-cell"
                  >
                    {formatTimestamp(c.last_seen)}
                  </Td>
                </LinkRow>
              );
            })}
          </TBody>
        </Table>
      </div>
    </Panel>
  );
}

/**
 * RegistryPanel — the contract-attribution registry. Reads the
 * /v1/protocols directory (each protocol's factory set and registered
 * contract count) and lists the protocols that own registered contracts,
 * factory contracts linked into their hub. The per-instance roster lives
 * on each /protocols/{name} page.
 */
function RegistryPanel() {
  const { data, isLoading, isError, refetch } = useQuery<RegistryProtocol[]>({
    queryKey: ['/v1/protocols', 'registry'],
    staleTime: 60_000,
    retry: false,
    queryFn: async () => {
      const env = await apiGet<{ data: { protocols: RegistryProtocol[] } }>(
        '/v1/protocols',
      );
      return env.data?.protocols ?? [];
    },
  });
  // Only protocols with a registry (a seeded factory or a non-zero contract
  // count), largest roster first. sdex and the external oracles carry
  // neither and are correctly omitted.
  const rows = (data ?? [])
    .filter((p) => p.contract_count > 0 || (p.factories?.length ?? 0) > 0)
    .sort((a, b) => b.contract_count - a.contract_count);

  return (
    <Panel
      headingLevel={2}
      title={
        rows.length > 0
          ? `Attributed protocols (${formatCompact(rows.length)})`
          : 'Registry'
      }
      hint={
        <span title="Each protocol owns contracts anchored to a verified factory, which lets us attribute an event to a protocol rather than a look-alike. Click a factory for its hub, or the count for the full roster.">
          factory-anchored attribution
        </span>
      }
      source={asExample('/v1/protocols')}
      bodyClassName="-mx-4 -mb-4"
    >
      {rows.length > 0 && <RegistryCharts rows={rows} />}
      <div className="overflow-x-auto">
        <Table>
          <THead>
            <tr>
              <Th>Protocol</Th>
              <Th>Category</Th>
              <Th>Factory contracts</Th>
              <Th align="right">Registered</Th>
              <Th align="right">Events (24h)</Th>
            </tr>
          </THead>
          <TBody>
            {isError && (
              <NoteRow colSpan={5}>
                Registry unavailable — the protocols read did not return.
                <Button
                  variant="secondary"
                  size="sm"
                  className="ml-3"
                  onClick={() => refetch()}
                >
                  Retry
                </Button>
              </NoteRow>
            )}
            {!isError && isLoading && (
              <TR className="hover:bg-transparent">
                <Td colSpan={5}>
                  <Skeleton className="h-4 w-64" />
                </Td>
              </TR>
            )}
            {!isError && !isLoading && rows.length === 0 && (
              <NoteRow colSpan={5}>
                No protocol contracts are registered yet.
              </NoteRow>
            )}
            {rows.map((p) => (
              <LinkRow
                href={`/protocols/${encodeURIComponent(p.name)}`}
                key={p.name}
              >
                <Td>
                  <Link
                    href={`/protocols/${encodeURIComponent(p.name)}`}
                    className="text-ink hover:text-brand-600 font-medium"
                  >
                    {protocolMeta(p.name)?.label ?? p.name}
                  </Link>
                </Td>
                <Td>
                  {p.category ? (
                    <Badge tone="neutral">{categoryLabel(p.category)}</Badge>
                  ) : (
                    <span className="text-ink-faint">—</span>
                  )}
                </Td>
                <Td>
                  {(p.factories?.length ?? 0) > 0 ? (
                    <div className="flex flex-wrap gap-x-3 gap-y-1">
                      {p.factories.map((f) => (
                        <Link
                          key={f}
                          href={`/contracts/${encodeURIComponent(f)}/`}
                          className="text-brand-600 font-mono text-xs hover:underline"
                          title={f}
                        >
                          {truncateMiddle(f, 6, 4)}
                        </Link>
                      ))}
                    </div>
                  ) : (
                    <span className="text-ink-faint">—</span>
                  )}
                </Td>
                <Td align="right" className="font-mono">
                  {p.contract_count > 0 ? (
                    <Link
                      href={`/protocols/${encodeURIComponent(p.name)}`}
                      className="text-brand-600 hover:underline"
                    >
                      {formatCompact(p.contract_count)}
                    </Link>
                  ) : (
                    <span className="text-ink-faint">—</span>
                  )}
                </Td>
                <Td align="right" className="font-mono">
                  {formatCompact(p.events_24h ?? 0)}
                </Td>
              </LinkRow>
            ))}
          </TBody>
        </Table>
      </div>
    </Panel>
  );
}
