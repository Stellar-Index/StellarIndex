'use client';

import { useMemo, useState } from 'react';
import { CURRENT_NETWORK_ID } from '@/lib/networks';
import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { apiGet, asExample } from '@/api/client';
import { formatCompact } from '@/lib/format';
import { hrefFor } from '@/lib/hrefFor';
import { Panel } from '@/components/reveal';
import {
  Badge,
  Callout,
  Container,
  EmptyState,
  PageHeader,
  Segmented,
  Skeleton,
  Stat,
  StatCell,
  StatGrid,
  TBody,
  LinkRow,
  TR,
  Table,
  Td,
  Th,
  THead,
} from '@/components/ui';
import { protocolMeta, PROTOCOLS, categoryLabel } from './registry';
import {
  ProtocolTvlPanel,
  type DexTvlTotal,
  type ProtocolTvl,
} from './ProtocolTvlPanel';

// Mirrors internal/api/v1/protocols.go ProtocolView.
interface ProtocolCard {
  name: string;
  category: string;
  description: string;
  genesis_ledger: number;
  factories: string[];
  contract_count: number;
  // Present on every live /v1/protocols row; left undefined on the
  // static-registry fallback cards (API unreachable) so the cells render
  // '—' rather than a fabricated "0 events" claim.
  events_24h?: number;
  completeness?: { complete: boolean; watermark_ledger: number };
  // Served on rows with a TVL derivation (the AMMs); absent elsewhere.
  tvl?: ProtocolTvl | null;
}

/**
 * ProtocolsIndex — the protocol directory: a grid of cards, one per
 * indexed protocol, fetched from /v1/protocols. Each card links into the
 * full /protocols/{name} analytics page. A category filter row scopes
 * the grid. Falls back to the static registry (always-rendered cards,
 * zeroed stats) when the directory endpoint is unreachable, so the
 * pillar never renders empty.
 *
 * When `lockedCategory` is set (e.g. the /bridges, /oracles category
 * landings), the grid is pinned to that one category, the chip row is
 * hidden, and the caller's header text is used — so a category page is a
 * one-liner over this component rather than a near-duplicate.
 */
export function ProtocolsIndex({
  lockedCategory,
  title = 'Protocols',
}: {
  lockedCategory?: string;
  title?: string;
} = {}) {
  // Several AMM protocols have no factory-seeded contract
  // roster yet (only blend is seeded; the ADR-0035 gates are pending
  // team answers) — their cards read "CONTRACTS 0" as if broken. The
  // per-source stats the /dexes page already uses carry the observed
  // active-pool count; fall back to that with an honest label.
  const { data: sourceStats } = useQuery<
    { name?: string; markets_count_24h?: number }[]
  >({
    queryKey: ['/v1/sources', 'stats', 'protocol-cards'],
    staleTime: 60_000,
    retry: false,
    queryFn: async () =>
      (
        await apiGet<{ data: { name?: string; markets_count_24h?: number }[] }>(
          '/v1/sources',
          { include: 'stats' },
        )
      ).data ?? [],
  });
  const poolsBySource = new Map(
    (sourceStats ?? []).map((s) => [s.name ?? '', s.markets_count_24h ?? 0]),
  );
  const [filter, setFilter] = useState<string>(lockedCategory ?? '');

  // The envelope carries the headline `tvl_total` ALONGSIDE `protocols[]`;
  // returning only the array here would discard it.
  // `tvl_total` is `omitempty` server-side — undefined is a real state
  // (nothing could be admitted into the sum), not a fetch failure, and is
  // rendered as absence rather than as a zero. See DexTvlHeadline.
  const {
    data: envelope,
    isError,
    isLoading,
  } = useQuery<{
    protocols: ProtocolCard[];
    tvlTotal?: DexTvlTotal;
  }>({
    queryKey: ['/v1/protocols'],
    retry: false,
    staleTime: 60_000,
    queryFn: async () => {
      const env = await apiGet<{
        data: { protocols: ProtocolCard[]; tvl_total?: DexTvlTotal };
      }>('/v1/protocols');
      return {
        protocols: env.data?.protocols ?? [],
        tvlTotal: env.data?.tvl_total,
      };
    },
  });
  const data = envelope?.protocols;

  // Fall back to the static registry so the grid renders even if the API
  // is down (the cards + links still work). Live stats are NOT fabricated
  // on the fallback: events_24h stays undefined and renders '—' — an
  // unreachable directory is not "0 events".
  const live = !!(data && data.length > 0);
  const cards: ProtocolCard[] = useMemo(() => {
    // Lean test nets: show only protocols that ACTUALLY exist on the network.
    // Measured on testnet + futurenet: blend has 3 contracts / 2
    // factories, every other protocol has 0. Those zeros are real absences —
    // contract_count comes from on-chain discovery, not from
    // stellarindex_enabled_sources — so rendering seven empty rows to surface
    // one populated one is noise. Mainnet keeps the full catalogue (a
    // zero-count protocol there is a genuine "indexed, nothing yet" signal).
    // The static PROTOCOLS fallback is skipped for the same reason: on a lean
    // net it would fabricate a full list of zero rows.
    if (data && data.length > 0) {
      return CURRENT_NETWORK_ID === 'mainnet'
        ? data
        : data.filter((c) => (c.contract_count ?? 0) > 0);
    }
    if (CURRENT_NETWORK_ID !== 'mainnet') return [];
    return PROTOCOLS.map((p) => ({
      name: p.name,
      category: '',
      description: p.description,
      genesis_ledger: 0,
      factories: [],
      contract_count: 0,
    }));
  }, [data]);

  const categories = useMemo(() => {
    const set = new Set<string>();
    for (const c of cards) if (c.category) set.add(c.category);
    return Array.from(set).sort();
  }, [cards]);

  const visible = useMemo(() => {
    const cat = lockedCategory ?? filter;
    return cat ? cards.filter((c) => c.category === cat) : cards;
  }, [cards, filter, lockedCategory]);

  // The headline describes the set the page actually shows — `visible`,
  // not the whole directory. Summing `cards` would make the category
  // landings publish the directory's totals over their own handful of
  // rows (e.g. "16 protocols · 1.15M events" for 2 bridges).
  const totalEvents24h = visible.reduce((s, c) => s + (c.events_24h ?? 0), 0);
  const verifiedCount = visible.filter((c) => c.completeness?.complete).length;
  // The static-registry fallback carries no category, so under a locked
  // category it matches nothing. That empty grid means the directory is
  // unreachable, not that we index no bridges — render it as absence,
  // the way the two live-only stats beside it already do.
  const protocolCount =
    live || !lockedCategory ? visible.length.toLocaleString('en-US') : '—';

  const sorted = useMemo(
    () =>
      [...visible].sort(
        (a, b) =>
          (b.events_24h ?? 0) - (a.events_24h ?? 0) ||
          a.name.localeCompare(b.name),
      ),
    [visible],
  );
  const showFilter = !lockedCategory && categories.length > 1;
  const sourceUrl = asExample('/v1/protocols').url;

  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[{ label: 'Home', href: '/' }, { label: title }]}
        title={title}
        meta="Protocols indexed from the raw ledger, with contract counts, 24h events and a coverage verdict per protocol."
      />
      <StatGrid cols={3}>
        <StatCell>
          <Stat label="Protocols" value={protocolCount} />
        </StatCell>
        <StatCell>
          <Stat label="Verified complete" value={live ? verifiedCount : '—'} />
        </StatCell>
        <StatCell>
          <Stat
            label="Events · last 24h"
            value={live ? formatCompact(totalEvents24h) : '—'}
          />
        </StatCell>
      </StatGrid>
      {isError && (
        <Callout tone="warn" title="Live stats unavailable">
          Showing the registry without live counts.
        </Callout>
      )}
      <ProtocolTvlPanel rows={visible} total={envelope?.tvlTotal} />
      <Panel
        headingLevel={2}
        title="Directory"
        hint={
          live
            ? `${visible.length.toLocaleString('en-US')} protocols · sorted by 24h events`
            : 'Static registry · live counts unavailable'
        }
        source={asExample('/v1/protocols')}
        bodyClassName="-mx-4 -mb-4"
      >
        {showFilter && (
          <div className="mx-4 mb-4">
            <Segmented
              ariaLabel="Category"
              className="flex-wrap"
              value={filter}
              onChange={setFilter}
              options={[
                { label: 'All', value: '' },
                ...categories.map((c) => ({
                  label: categoryLabel(c),
                  value: c,
                })),
              ]}
            />
          </div>
        )}
        <div data-source={sourceUrl}>
          <div className="overflow-x-auto">
            <Table>
              <THead>
                <tr>
                  <Th>Protocol</Th>
                  <Th className="hidden sm:table-cell">Category</Th>
                  <Th align="right">Contracts</Th>
                  <Th align="right">Pools · 24h</Th>
                  <Th align="right">Events · 24h</Th>
                  <Th>Coverage</Th>
                </tr>
              </THead>
              <TBody>
                {isLoading && visible.length === 0 && (
                  <TR>
                    <Td colSpan={6}>
                      <Skeleton className="h-5 w-48" />
                    </Td>
                  </TR>
                )}
                {sorted.map((c) => {
                  const label = protocolMeta(c.name)?.label ?? c.name;
                  const href =
                    c.name === 'sdex' ? '/sdex' : hrefFor.protocol(c.name);
                  const pools = poolsBySource.get(c.name) ?? 0;
                  return (
                    <LinkRow href={href} key={c.name}>
                      <Td>
                        <Link
                          href={href}
                          className="text-ink hover:text-brand-600 font-medium"
                        >
                          {label}
                        </Link>
                        {c.description && (
                          <div className="text-ink-muted line-clamp-1 max-w-md text-xs">
                            {c.description}
                          </div>
                        )}
                      </Td>
                      <Td className="text-ink-muted hidden sm:table-cell">
                        {c.category ? categoryLabel(c.category) : '—'}
                      </Td>
                      <Td align="right" className="font-mono tabular-nums">
                        {c.contract_count > 0 ? (
                          c.contract_count.toLocaleString('en-US')
                        ) : (
                          <span className="text-ink-faint">—</span>
                        )}
                      </Td>
                      <Td align="right" className="font-mono tabular-nums">
                        {c.name === 'sdex' ? (
                          <span className="text-ink-faint font-sans">
                            order book
                          </span>
                        ) : pools > 0 ? (
                          pools.toLocaleString('en-US')
                        ) : (
                          <span className="text-ink-faint">—</span>
                        )}
                      </Td>
                      <Td align="right" className="font-mono tabular-nums">
                        {c.events_24h != null ? (
                          formatCompact(c.events_24h)
                        ) : (
                          <span className="text-ink-faint">—</span>
                        )}
                      </Td>
                      <Td>
                        <CoverageBadge completeness={c.completeness} />
                      </Td>
                    </LinkRow>
                  );
                })}
              </TBody>
            </Table>
          </div>
          {!isLoading && visible.length === 0 && (
            <div className="px-4 pb-4">
              {isError ? (
                <EmptyState
                  title="Protocol directory unavailable right now"
                  description="The registry could not be reached. Try again shortly."
                />
              ) : (
                <EmptyState
                  title="No protocols in this category"
                  description="Nothing indexed under this category yet."
                />
              )}
            </div>
          )}
        </div>
      </Panel>
    </Container>
  );
}

function CoverageBadge({
  completeness,
}: {
  completeness?: { complete: boolean; watermark_ledger: number };
}) {
  if (!completeness) return <Badge>unknown</Badge>;
  const title = `Watermark ledger ${completeness.watermark_ledger.toLocaleString('en-US')}`;
  return completeness.complete ? (
    <Badge tone="ok" dot title={title}>
      complete
    </Badge>
  ) : (
    <Badge tone="warn" dot title={title}>
      partial
    </Badge>
  );
}
