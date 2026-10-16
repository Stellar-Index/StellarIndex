'use client';

import { useMemo, useState } from 'react';
import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { formatCompact } from '@/lib/format';
import { dropPartialTrailingDay } from '@/lib/series';
import { CopyHash, relativeAge, formatTimestamp } from '../../explorer-shared';
import {
  categoryLabel,
  PROTOCOLS,
  type ProtocolRegistryEntry,
} from '../registry';
import { TimeSeriesChart } from './TimeSeriesChart';
import { AnalyticsStatusNote, BespokeUnavailable } from './AnalyticsStatusNote';
import { BespokeSection, type Bespoke } from './BespokeSection';
import { ExternalRef } from '@/components/ExternalRef';
import type { paths } from '@/api/types';
import {
  Badge,
  Button,
  ButtonLink,
  Container,
  PageHeader,
  Segmented,
  Skeleton,
  Stat,
  StatCell,
  StatGrid,
  TBody,
  Table,
  Td,
  Th,
  THead,
  LinkRow,
} from '@/components/ui';
import { PANEL_LABEL_CLASS } from '@/components/ui/Page';

// ─── Wire shapes — derived from the generated OpenAPI contract
// (src/api/types.ts, `make web-generate-api`); mirror
// internal/api/v1/protocols.go ProtocolDetailView. ───

type ProtocolDetailWire = NonNullable<
  paths['/protocols/{name}']['get']['responses'][200]['content']['application/json']['data']
>;

type ProtocolContract = ProtocolDetailWire['contracts'][number];

type EventTypeCount = NonNullable<
  ProtocolDetailWire['event_breakdown']
>[number];

type ProtocolDetail = ProtocolDetailWire & {
  // The spec documents `bespoke` as a free-form object; typed precisely
  // here (internal/api/v1/protocols.go ProtocolBespoke, omitempty — absent
  // when no bespoke reader is wired or the category has none yet).
  bespoke?: Bespoke;
};

const LINK_CLASS = 'text-brand-600 font-medium hover:underline';

/**
 * ProtocolView — the per-protocol analytics page: header → KPI strip →
 * bespoke analytics → activity → event types → contracts → identity. The
 * lake-analytics fields are omitempty on the wire when the lake reader is
 * down; every section degrades to "unavailable" while the registry and the
 * contract roster still render from the always-served halves.
 */
export function ProtocolView({
  name,
  label,
  children,
}: {
  name: string;
  label: string;
  /** Page-specific sections, rendered above the references footer. */
  children?: React.ReactNode;
}) {
  const { data, isLoading, isError, error, refetch } = useQuery<ProtocolDetail>(
    {
      queryKey: ['/v1/protocols/{name}', name],
      retry: false,
      staleTime: 60_000,
      queryFn: async () => {
        const env = await apiGet<{ data: ProtocolDetail }>(
          `/v1/protocols/${encodeURIComponent(name)}`,
        );
        return env.data;
      },
    },
  );

  const source = asExample(`/v1/protocols/${name}`);
  const registry = PROTOCOLS.find((p) => p.name === name);

  if (isError) {
    return (
      <Shell name={name} label={label} description={registry?.description}>
        <Panel
          headingLevel={2}
          title="Couldn't load this protocol"
          source={source}
        >
          <p className="text-ink-muted text-sm">
            The protocol directory is unreachable right now:{' '}
            {error instanceof Error ? error.message : 'unknown error'}.
          </p>
          <div className="mt-3 flex gap-2">
            <Button variant="secondary" size="sm" onClick={() => refetch()}>
              Retry
            </Button>
            <ButtonLink variant="ghost" size="sm" href="/status">
              Status page
            </ButtonLink>
          </div>
        </Panel>
      </Shell>
    );
  }

  if (isLoading || !data) {
    return (
      <Shell name={name} label={label} description={registry?.description}>
        <Skeleton className="h-24" aria-hidden />
        <Skeleton className="h-72" aria-hidden />
      </Shell>
    );
  }

  const analyticsAvailable =
    data.activity_window_days != null && data.activity_window_days > 0;
  const windowDays = data.activity_window_days ?? 0;
  // SDEX is protocol-native: it has no contracts, so a 0 is noise.
  const hasContracts = data.contract_count > 0 || data.factories.length > 0;

  return (
    <Shell
      name={name}
      label={label}
      description={registry?.description}
      actions={
        <>
          <Badge>{categoryLabel(data.category)}</Badge>
          <CompletenessBadge completeness={data.completeness} />
        </>
      }
      links={<ProtocolCrossLinks name={name} category={data.category} />}
    >
      <StatGrid cols={hasContracts ? 5 : 2}>
        {hasContracts && (
          <StatCell>
            <Stat
              label="Contracts"
              value={formatCompact(data.contract_count)}
            />
          </StatCell>
        )}
        {/* The windowed count is over contract events; a protocol with no
            contracts (SDEX) has none to count, which is not an outage. */}
        {hasContracts && (
          <StatCell>
            <Stat
              label={analyticsAvailable ? `Events · ${windowDays}d` : 'Events'}
              value={
                analyticsAvailable && data.events_total != null
                  ? formatCompact(data.events_total)
                  : '—'
              }
              sub={
                analyticsAvailable ? undefined : 'Lake analytics unavailable'
              }
            />
          </StatCell>
        )}
        <StatCell>
          <Stat label="Events · 24h" value={formatCompact(data.events_24h)} />
        </StatCell>
        {hasContracts && (
          <StatCell>
            <Stat
              label="Factories"
              value={formatCompact(data.factories.length)}
            />
          </StatCell>
        )}
        <StatCell>
          <Stat
            label="Genesis ledger"
            value={<span className="font-mono">#{data.genesis_ledger}</span>}
          />
        </StatCell>
      </StatGrid>

      {/* Explicit server status — never inferred from field absence. */}
      <AnalyticsStatusNote analytics={data.analytics} />

      {/* Absent + "unavailable" ⇒ the build degraded — say so instead of
          rendering nothing; absent under "ok" ⇒ the category has no suite. */}
      {data.bespoke ? (
        <BespokeSection bespoke={data.bespoke} source={source} name={name} />
      ) : data.analytics?.status === 'unavailable' ? (
        <BespokeUnavailable />
      ) : null}

      {/* Without a lake window the KPI strip already says so; two more
          panels saying it again would be chrome around nothing. */}
      {analyticsAvailable && (
        <>
          <Panel
            headingLevel={2}
            title="On-chain activity"
            hint={`Events per day · last ${windowDays}d`}
            source={source}
          >
            {(data.activity_series?.length ?? 0) === 0 ? (
              <Empty text="No on-chain activity in the window." />
            ) : (
              <TimeSeriesChart
                // Today's accumulating UTC bucket is dropped — a partial day
                // plotted as a full one ends the series on a phantom cliff.
                points={dropPartialTrailingDay(data.activity_series ?? []).map(
                  (p) => ({
                    date: p.date ?? '',
                    value: p.events ?? 0,
                  }),
                )}
                label="Daily on-chain events"
                unit="events"
              />
            )}
          </Panel>

          <Panel
            headingLevel={2}
            title="Event types"
            hint={`Share of ${formatCompact(data.events_total ?? 0)} events · last ${windowDays}d`}
            source={source}
          >
            {(data.event_breakdown?.length ?? 0) === 0 ? (
              <Empty text="No decoded events in the window." />
            ) : (
              <EventBreakdown
                breakdown={data.event_breakdown ?? []}
                total={data.events_total ?? 0}
              />
            )}
          </Panel>
        </>
      )}

      <ContractRoster
        contracts={data.contracts}
        contractCount={data.contract_count}
        analyticsAvailable={analyticsAvailable}
        source={source}
      />

      <Identity data={data} />

      {children}

      <ProtocolFooter
        links={registry?.links}
        verificationPage={data.verification_page}
      />
    </Shell>
  );
}

// ─── Shell ───────────────────────────────────────────────────────────────

function Shell({
  name,
  label,
  description,
  actions,
  links,
  children,
}: {
  name: string;
  label: string;
  description?: string;
  actions?: React.ReactNode;
  links?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[
          { label: 'Home', href: '/' },
          { label: 'Protocols', href: '/protocols' },
          { label: label || name },
        ]}
        title={label}
        meta={description}
        actions={actions}
      />
      {links}
      {children}
    </Container>
  );
}

// Sources with a dedicated DEX detail page (/dexes/[source]).
const DEX_PAGES = new Set(['soroswap', 'phoenix', 'aquarius', 'sdex', 'comet']);

function ProtocolCrossLinks({
  name,
  category,
}: {
  name: string;
  category: string;
}) {
  const isDex = category === 'dex' || category === 'amm';
  return (
    <div className="flex flex-wrap gap-x-4 gap-y-1 text-sm">
      <Link
        href={`/sources/${encodeURIComponent(name)}`}
        className={LINK_CLASS}
      >
        Source registry detail
      </Link>
      {isDex && DEX_PAGES.has(name) && (
        <Link
          href={`/dexes/${encodeURIComponent(name)}`}
          className={LINK_CLASS}
        >
          Pools and chart
        </Link>
      )}
      {category === 'lending' && (
        <Link href="/lending" className={LINK_CLASS}>
          Lending pools
        </Link>
      )}
      {category === 'oracle' && (
        <Link href="/oracles" className={LINK_CLASS}>
          Oracle feeds
        </Link>
      )}
    </div>
  );
}

function CompletenessBadge({
  completeness,
}: {
  completeness?: ProtocolDetail['completeness'];
}) {
  if (!completeness) {
    return (
      <Badge title="No completeness verdict recorded for this source yet.">
        Coverage unknown
      </Badge>
    );
  }
  const ledger = `#${completeness.watermark_ledger}`;
  if (completeness.complete) {
    return (
      <Badge
        tone="ok"
        dot
        title={`Verified complete to ledger ${ledger}: archive hash-chained, every event recognised, served tier reconciled.`}
      >
        Verified complete
      </Badge>
    );
  }
  return (
    <Badge tone="warn" dot title={`Partial coverage to ledger ${ledger}.`}>
      Partial coverage
    </Badge>
  );
}

// ─── Event-type breakdown ────────────────────────────────────────────────────

// How many bars to show before collapsing behind a "Show N more" control.
const EVENT_BREAKDOWN_TOP_N = 8;

function EventBreakdown({
  breakdown,
  total,
}: {
  breakdown: EventTypeCount[];
  total: number;
}) {
  const [expanded, setExpanded] = useState(false);
  const max = breakdown.reduce((m, b) => Math.max(m, b.count ?? 0), 0);
  const overflow = breakdown.length - EVENT_BREAKDOWN_TOP_N;
  const visible =
    expanded || overflow <= 0
      ? breakdown
      : breakdown.slice(0, EVENT_BREAKDOWN_TOP_N);

  return (
    <div className="space-y-3">
      <ul className="space-y-2">
        {visible.map((b) => {
          const count = b.count ?? 0;
          const pct = total > 0 ? (count / total) * 100 : 0;
          const barPct = max > 0 ? (count / max) * 100 : 0;
          return (
            <li key={b.event_type}>
              <div className="mb-1 flex items-baseline justify-between gap-3 text-xs">
                <span
                  className="text-ink truncate font-mono"
                  title={b.event_type}
                >
                  {b.event_type}
                </span>
                <span className="text-ink-muted tnum shrink-0">
                  <span className="text-ink font-mono">
                    {formatCompact(count)}
                  </span>{' '}
                  · {pct.toFixed(pct >= 10 ? 0 : 1)}%
                </span>
              </div>
              <div
                className="bg-surface-subtle h-1.5 overflow-hidden rounded-sm"
                role="img"
                aria-label={`${b.event_type}: ${count} events, ${pct.toFixed(1)}% of total`}
              >
                <div
                  className="bg-brand-500 h-full"
                  style={{ width: `${Math.max(barPct, 1)}%` }}
                />
              </div>
            </li>
          );
        })}
      </ul>
      {overflow > 0 && (
        <Button
          variant="ghost"
          size="sm"
          onClick={() => setExpanded((v) => !v)}
          aria-expanded={expanded}
        >
          {expanded
            ? 'Show fewer'
            : `Show ${overflow} more event ${overflow === 1 ? 'type' : 'types'}`}
        </Button>
      )}
    </div>
  );
}

// ─── Contract roster ─────────────────────────────────────────────────────────

type RosterSortKey = 'events' | 'last_seen';

const ROSTER_SORT: { label: string; value: RosterSortKey }[] = [
  { label: 'Most events', value: 'events' },
  { label: 'Last seen', value: 'last_seen' },
];

// Instances shown before the "Show N more" control collapses the long tail.
const ROSTER_TOP_N = 25;

function ContractRoster({
  contracts,
  contractCount,
  analyticsAvailable,
  source,
}: {
  contracts: ProtocolContract[];
  contractCount: number;
  analyticsAvailable: boolean;
  source: ReturnType<typeof asExample>;
}) {
  const [sortKey, setSortKey] = useState<RosterSortKey>('events');
  const [expanded, setExpanded] = useState(false);

  // The per-contract analytics (events / last_seen) are a SEPARATE lake
  // fill from the window-level analytics (fillProtocolContractActivity vs
  // fillProtocolSeries in internal/api/v1/protocols.go) — either can
  // degrade alone, and `events` is omitempty on the wire, so a degraded
  // fill leaves every roster row bare ({contract_id, kind} only) even
  // when activity_window_days is set. Rendering '0' for those rows would
  // claim "this contract emitted nothing", which we cannot know. Only
  // when at least one row carries the analytics fields did the fill run —
  // then a bare `events` is a genuine present-and-zero.
  const rosterHasActivity = contracts.some(
    (c) => c.events != null || c.last_seen != null,
  );

  const { factories, instances } = useMemo(() => {
    const f = contracts.filter((c) => c.kind === 'factory');
    const cmp =
      sortKey === 'events'
        ? (a: ProtocolContract, b: ProtocolContract) =>
            (b.events ?? 0) - (a.events ?? 0)
        : (a: ProtocolContract, b: ProtocolContract) =>
            (b.last_seen ?? '').localeCompare(a.last_seen ?? '');
    const i = contracts
      .filter((c) => c.kind !== 'factory')
      .slice()
      .sort(cmp);
    return { factories: f, instances: i };
  }, [contracts, sortKey]);

  const hasTokens = contracts.some((c) => c.pair || c.token0 || c.token1);
  // Factories always render (there are few and they anchor the protocol).
  const visibleInstances =
    expanded || instances.length <= ROSTER_TOP_N
      ? instances
      : instances.slice(0, ROSTER_TOP_N);
  const overflow = instances.length - visibleInstances.length;

  if (contracts.length === 0) {
    // An empty roster beside a non-zero contract_count is not a
    // contradiction: a protocol whose instances outnumber the API's roster
    // cap (sorocredit's per-position child contracts) is counted rather than
    // enumerated, so the count is exact and the list is simply not published.
    return (
      <p className="text-ink-muted text-sm">
        {contractCount > 0
          ? `${formatCompact(contractCount)} contracts are counted, not listed: the roster is published only where the instance set is small enough to enumerate. The count is the exact total.`
          : 'This source has no contract registry. It is a classic-protocol venue, an event-less oracle, or a bridge tracked without a factory model.'}
      </p>
    );
  }

  return (
    <Panel
      headingLevel={2}
      title="Contracts"
      hint={`${contracts.length} listed · ${factories.length} ${factories.length === 1 ? 'factory' : 'factories'} · ${instances.length} ${instances.length === 1 ? 'instance' : 'instances'}`}
      source={source}
      bodyClassName="-mx-4 -mb-4"
    >
      <div className="mx-4 mb-4">
        <Segmented
          ariaLabel="Sort"
          options={ROSTER_SORT}
          value={sortKey}
          onChange={(v) => setSortKey(v as RosterSortKey)}
        />
      </div>
      <div className="overflow-x-auto">
        <Table>
          <THead>
            <tr>
              <Th>Role</Th>
              <Th>Contract</Th>
              {hasTokens && <Th>Pair</Th>}
              <Th align="right">Events</Th>
              <Th align="right">Last seen</Th>
            </tr>
          </THead>
          <TBody>
            {[...factories, ...visibleInstances].map((c) => (
              <LinkRow
                href={`/contracts/${encodeURIComponent(c.contract_id)}/`}
                key={c.contract_id}
              >
                <Td>
                  <RoleBadge kind={c.kind} />
                </Td>
                <Td>
                  <Link
                    href={`/contracts/${encodeURIComponent(c.contract_id)}/`}
                    className="text-brand-600 font-mono text-xs hover:underline"
                  >
                    <CopyHash value={c.contract_id} head={8} tail={6} />
                  </Link>
                </Td>
                {hasTokens && (
                  <Td>
                    {c.pair ? (
                      // The human pair ("XLM/USDC") leads; the raw token
                      // contracts hang off the tooltip for the on-chain reader.
                      <span
                        className="text-ink font-medium"
                        title={(c.tokens ?? []).join(' · ')}
                      >
                        {c.pair}
                      </span>
                    ) : c.token0 || c.token1 ? (
                      <span className="text-ink-muted font-mono text-xs">
                        {shortId(c.token0)} / {shortId(c.token1)}
                      </span>
                    ) : (
                      <span className="text-ink-faint">—</span>
                    )}
                  </Td>
                )}
                <Td align="right" className="font-mono">
                  {c.events != null && c.events > 0 ? (
                    formatCompact(c.events)
                  ) : (
                    <span className="text-ink-faint">
                      {analyticsAvailable && rosterHasActivity ? '0' : '—'}
                    </span>
                  )}
                </Td>
                <Td
                  align="right"
                  className="text-ink-muted font-mono text-xs"
                  title={c.last_seen ? formatTimestamp(c.last_seen) : undefined}
                >
                  {c.last_seen ? (
                    relativeAge(c.last_seen)
                  ) : (
                    <span className="text-ink-faint">—</span>
                  )}
                </Td>
              </LinkRow>
            ))}
          </TBody>
        </Table>
      </div>
      {instances.length > ROSTER_TOP_N && (
        <div className="border-line border-t px-4 py-3">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setExpanded((v) => !v)}
            aria-expanded={expanded}
          >
            {expanded
              ? 'Show fewer'
              : `Show ${overflow} more ${overflow === 1 ? 'instance' : 'instances'}`}
          </Button>
        </div>
      )}
    </Panel>
  );
}

function RoleBadge({ kind }: { kind?: string }) {
  if (kind === 'factory') return <Badge tone="brand">factory</Badge>;
  if (kind === 'module') {
    return (
      <Badge title="A sub-module contract that belongs to this protocol but emits on its own address (e.g. the Blend Backstop insurance module).">
        module
      </Badge>
    );
  }
  return <span className="text-ink-muted text-xs">instance</span>;
}

// ─── Identity + footer ───────────────────────────────────────────────────────

function Identity({ data }: { data: ProtocolDetail }) {
  if (data.factories.length === 0 && data.event_kinds.length === 0) return null;
  return (
    <Panel headingLevel={2} title="Protocol identity" bodyClassName="space-y-4">
      {data.factories.length > 0 && (
        <div>
          <div className={PANEL_LABEL_CLASS}>
            Verified factories · {data.factories.length}
          </div>
          <ul className="mt-2 flex flex-wrap gap-x-4 gap-y-1">
            {data.factories.map((f) => (
              <li key={f}>
                <Link
                  href={`/contracts/${encodeURIComponent(f)}/`}
                  className="text-brand-600 font-mono text-xs hover:underline"
                  title={f}
                >
                  {shortId(f)}
                </Link>
              </li>
            ))}
          </ul>
        </div>
      )}
      {data.event_kinds.length > 0 && (
        <div>
          <div className={PANEL_LABEL_CLASS}>
            Decoder event vocabulary · {data.event_kinds.length}
          </div>
          <p className="text-ink-body mt-2 font-mono text-xs leading-relaxed">
            {data.event_kinds.join(' · ')}
          </p>
        </div>
      )}
    </Panel>
  );
}

function ProtocolFooter({
  links,
  verificationPage,
}: {
  links?: ProtocolRegistryEntry['links'];
  verificationPage?: string;
}) {
  const refs = [links?.website, links?.docs, links?.source].filter(
    (h): h is string => !!h,
  );
  if (refs.length === 0 && !verificationPage) return null;
  return (
    <div className="border-line flex flex-wrap gap-x-4 gap-y-1 border-t pt-4 text-xs">
      {refs.map((href) => (
        <ExternalRef key={href} href={href} />
      ))}
      {verificationPage && (
        <ExternalRef
          href={`https://github.com/Stellar-Index/StellarIndex/blob/main/${verificationPage}`}
        >
          Verification write-up
        </ExternalRef>
      )}
    </div>
  );
}

// ─── Empty / unavailable states ──────────────────────────────────────────────

function Empty({ text }: { text: string }) {
  return <p className="text-ink-muted py-6 text-center text-sm">{text}</p>;
}

function shortId(id?: string): string {
  if (!id) return '—';
  if (id.length <= 14) return id;
  return `${id.slice(0, 6)}…${id.slice(-4)}`;
}
