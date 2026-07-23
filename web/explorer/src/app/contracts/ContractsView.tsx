'use client';

import { useState } from 'react';
import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { useSACWrappers } from '@/api/hooks';
import { formatCompact } from '@/lib/format';
import { useTableSort, SortableTh, type SortColumn } from '@/lib/useTableSort';
import { Container, PageHeader, Segmented } from '@/components/ui';
import { categoryTone, protocolMeta } from '../protocols/registry';
import { formatTimestamp } from '../explorer-shared';
import type { paths } from '@/api/types';

// GET /v1/contracts response body, derived from the generated OpenAPI
// contract (src/api/types.ts, `make web-generate-api`).
type DirectoryResp = NonNullable<
  paths['/contracts']['get']['responses'][200]['content']['application/json']['data']
>;

// Mirrors internal/api/v1/protocols.go ProtocolView — the slice the registry
// tab needs (the /v1/protocols directory carries the contract-attribution
// registry: which contracts each protocol's factory owns, and how many).
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
 *     Answers "what's busy on Soroban right now". Dominated by SACs and
 *     high-traffic system contracts, so its protocol column is mostly "—":
 *     raw activity and protocol attribution are orthogonal.
 *   - "Registry" — the contracts the protocol registry actually attributes
 *     (GET /v1/protocols), grouped by protocol, with each factory contract
 *     linked into its hub. Answers "which contracts does Stellar Index
 *     vouch for as belonging to Blend / Soroswap / …" (CON-1: the protocol
 *     column was empty on every event-ranked row because attribution lives
 *     here, not in the activity leaderboard).
 */
export function ContractsView() {
  const { data: sacMap } = useSACWrappers();
  const [view, setView] = useState<'active' | 'registry'>('active');

  return (
    <Container className="space-y-8 py-8 sm:py-10">
      <PageHeader
        eyebrow="Soroban"
        title="Contracts"
        description="Every Soroban contract, two ways: the most active over the last 30 days (ranked by emitted events), and the attribution registry — the contracts each protocol's factory owns. Click any contract for its hub: events, decoded code, and cross-contract interaction map."
      />

      <div className="flex items-center gap-3">
        <Segmented
          value={view}
          onChange={(v) => setView(v as 'active' | 'registry')}
          options={[
            { label: 'Most active', value: 'active' },
            { label: 'Registry', value: 'registry' },
          ]}
        />
        <span className="text-xs text-ink-muted">
          {view === 'active'
            ? 'Ranked by 30-day event volume'
            : 'Contracts attributed to a protocol (ADR-0035)'}
        </span>
      </div>

      {view === 'active' ? (
        <MostActivePanel sacMap={sacMap} />
      ) : (
        <RegistryPanel />
      )}

      <p className="text-xs text-ink-muted">
        Looking for a specific contract? Paste its <code className="font-mono">C…</code>{' '}
        address into search, or open it directly at{' '}
        <code className="font-mono">/contracts/&lt;C…&gt;</code>.
      </p>
    </Container>
  );
}

function MostActivePanel({ sacMap }: { sacMap: SACMap }) {
  const { data, isLoading, isError, error } = useQuery<DirectoryResp>({
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

  // Sortable columns (site-audit S36); default keeps the API's
  // events-ranked order until a header is clicked.
  const contractSortColumns: SortColumn<(typeof rows)[number], string>[] = [
    { key: 'protocol', value: (c) => c.protocol ?? '', initialDir: 'asc' },
    { key: 'events', value: (c) => c.events },
    { key: 'last_seen', value: (c) => c.last_seen ?? '' },
  ];
  const {
    sorted: sortedContracts,
    sort: cSort,
    toggle: cToggle,
    ariaSort: cAriaSort,
  } = useTableSort<(typeof rows)[number], string>(rows, contractSortColumns, null);

  return (
    <Panel
      title={rows.length > 0 ? `Most active (${formatCompact(rows.length)})` : 'Most active'}
      source={asExample('/v1/contracts', { days: 30, limit: 100 })}
      bodyClassName="-mx-4"
    >
      <p className="px-4 pb-3 text-xs text-ink-muted">
        Ranked by raw event volume, so the leaders are SACs and high-traffic
        system contracts — attribution lives in the{' '}
        <span className="font-medium text-ink-body">Registry</span> view, not
        here, so most rows below carry no protocol tag by design.
      </p>
      {isError ? (
        <p className="px-4 text-sm text-down-strong">
          Failed to load contracts:{' '}
          {error instanceof Error ? error.message : 'unknown error'}
        </p>
      ) : isLoading ? (
        <p className="px-4 text-sm text-ink-muted">Loading…</p>
      ) : rows.length === 0 ? (
        <p className="px-4 text-sm text-ink-muted">
          No contract activity in the window.
        </p>
      ) : (
        <div className="overflow-x-auto">
          <table className="min-w-full divide-y divide-line text-sm">
            <thead>
              <tr className="text-left text-[11px] uppercase tracking-wider text-ink-muted">
                <th scope="col" className="px-4 py-2">Contract</th>
                <SortableTh label="Protocol" sortKey="protocol" sort={cSort} onSort={cToggle} ariaSort={cAriaSort} />
                <SortableTh label="Events (30d)" sortKey="events" sort={cSort} onSort={cToggle} ariaSort={cAriaSort} align="right" />
                <SortableTh label="Last seen" sortKey="last_seen" sort={cSort} onSort={cToggle} ariaSort={cAriaSort} />
              </tr>
            </thead>
            <tbody className="divide-y divide-line-subtle">
              {sortedContracts.map((c) => (
                <tr key={c.contract_id} className="hover:bg-surface-muted">
                  <td className="px-4 py-3">
                    <Link
                      href={`/contracts/${encodeURIComponent(c.contract_id ?? '')}/`}
                      className="font-mono text-xs text-brand-600 hover:underline"
                      title={c.contract_id}
                    >
                      {(c.contract_id ?? '').slice(0, 8)}…{(c.contract_id ?? '').slice(-6)}
                    </Link>
                    {/* CON-2: 2 of the top 3 rows are SACs resolvable
                        from the operator wrapper map already cached
                        sitewide — name them instead of bare hashes. */}
                    {(() => {
                      const wrapped =
                        c.contract_id === 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA'
                          ? 'native'
                          : sacMap?.[c.contract_id ?? ''];
                      if (!wrapped) return null;
                      const code = wrapped === 'native' ? 'XLM' : wrapped.split(/[:-]/)[0];
                      return (
                        <span className="ml-2 rounded-sm bg-surface-muted px-1.5 py-0.5 text-[9px] font-medium uppercase tracking-wider text-ink-muted">
                          {code} SAC
                        </span>
                      );
                    })()}
                  </td>
                  <td className="px-4 py-3">
                    {c.protocol ? (
                      <Link
                        href={`/protocols/${encodeURIComponent(c.protocol)}`}
                        className="inline-flex items-center rounded-sm px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-wider bg-brand-100 text-brand-800 hover:bg-brand-200"
                      >
                        {c.protocol}
                      </Link>
                    ) : (
                      <span className="text-ink-faint">—</span>
                    )}
                  </td>
                  <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-body">
                    {formatCompact(c.events ?? 0)}
                  </td>
                  <td className="px-4 py-3 text-xs text-ink-muted">
                    {formatTimestamp(c.last_seen)}
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
 * RegistryPanel — the contract-attribution registry (CON-1). Reads the
 * /v1/protocols directory (which carries each protocol's factory set +
 * registered-contract count) and lists the protocols that actually own
 * registered contracts, factory contracts linked into their hub. The full
 * per-instance roster lives on each /protocols/{name} page.
 */
function RegistryPanel() {
  const { data, isLoading, isError, error } = useQuery<RegistryProtocol[]>({
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

  // Only protocols with an actual registry (a seeded factory or a non-zero
  // contract count). Sort by roster size so the attribution-heavy protocols
  // lead. sdex / external oracles carry neither and are (correctly) omitted.
  const rows = (data ?? [])
    .filter((p) => p.contract_count > 0 || (p.factories?.length ?? 0) > 0)
    .sort((a, b) => b.contract_count - a.contract_count);

  return (
    <Panel
      title={rows.length > 0 ? `Attributed protocols (${formatCompact(rows.length)})` : 'Registry'}
      source={asExample('/v1/protocols')}
      bodyClassName="-mx-4"
    >
      <p className="px-4 pb-3 text-xs text-ink-muted">
        Each protocol below owns a set of contracts anchored to a verified
        factory (ADR-0035) — the identity hinge that lets us attribute an
        event to a protocol rather than a look-alike. Click a factory to open
        its hub, or the count to see the full contract roster.
      </p>
      {isError ? (
        <p className="px-4 text-sm text-down-strong">
          Failed to load the registry:{' '}
          {error instanceof Error ? error.message : 'unknown error'}
        </p>
      ) : isLoading ? (
        <p className="px-4 text-sm text-ink-muted">Loading…</p>
      ) : rows.length === 0 ? (
        <p className="px-4 text-sm text-ink-muted">
          No protocol contracts are registered yet.
        </p>
      ) : (
        <div className="overflow-x-auto">
          <table className="min-w-full divide-y divide-line text-sm">
            <thead>
              <tr className="text-left text-[11px] uppercase tracking-wider text-ink-muted">
                <th scope="col" className="px-4 py-2">Protocol</th>
                <th scope="col" className="px-4 py-2">Category</th>
                <th scope="col" className="px-4 py-2">Factory contracts</th>
                <th scope="col" className="px-4 py-2 text-right">Registered</th>
                <th scope="col" className="px-4 py-2 text-right">Events (24h)</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-line-subtle">
              {rows.map((p) => (
                <tr key={p.name} className="hover:bg-surface-muted">
                  <td className="px-4 py-3">
                    <Link
                      href={`/protocols/${encodeURIComponent(p.name)}`}
                      className="font-medium text-brand-600 hover:underline"
                    >
                      {protocolMeta(p.name)?.label ?? p.name}
                    </Link>
                  </td>
                  <td className="px-4 py-3">
                    {p.category ? (
                      <span
                        className={`inline-flex items-center rounded-sm px-1.5 py-0.5 font-mono text-[9px] uppercase tracking-wider ${categoryTone(p.category)}`}
                      >
                        {p.category}
                      </span>
                    ) : (
                      <span className="text-ink-faint">—</span>
                    )}
                  </td>
                  <td className="px-4 py-3">
                    {(p.factories?.length ?? 0) > 0 ? (
                      <div className="flex flex-wrap gap-1.5">
                        {p.factories.map((f) => (
                          <Link
                            key={f}
                            href={`/contracts/${encodeURIComponent(f)}/`}
                            className="font-mono text-[11px] text-brand-600 hover:underline"
                            title={f}
                          >
                            {f.slice(0, 6)}…{f.slice(-4)}
                          </Link>
                        ))}
                      </div>
                    ) : (
                      <span className="text-ink-faint">—</span>
                    )}
                  </td>
                  <td className="px-4 py-3 text-right">
                    {p.contract_count > 0 ? (
                      <Link
                        href={`/protocols/${encodeURIComponent(p.name)}`}
                        className="font-mono tabular-nums text-brand-600 hover:underline"
                      >
                        {formatCompact(p.contract_count)}
                      </Link>
                    ) : (
                      <span className="font-mono tabular-nums text-ink-faint">—</span>
                    )}
                  </td>
                  <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-body">
                    {formatCompact(p.events_24h ?? 0)}
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
