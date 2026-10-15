'use client';

import Link from 'next/link';
import { useMemo, useState, type ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';

import { hrefFor } from '@/lib/hrefFor';
import { Panel } from '@/components/reveal';
import { AssetText } from '@/components/AssetLink';
import { apiGet, asExample } from '@/api/client';
import {
  isRawOracleAsset,
  rawOracleSymbol,
  shortAssetText,
} from '@/lib/asset-label';
import { formatOraclePrice, formatRelative } from '@/lib/format';
// Wire shapes from the generated OpenAPI contract: /v1/sources rows via
// the shared hooks alias; /v1/oracle/streams rows are the spec's
// OracleReading schema.
import type { Source as SourceRow } from '@/api/hooks';
import type { components } from '@/api/types';
import {
  Badge,
  Button,
  Container,
  PageHeader,
  Segmented,
  Skeleton,
  TBody,
  TR,
  Table,
  Td,
  Th,
  THead,
  LinkRow,
} from '@/components/ui';

type OracleStream = components['schemas']['OracleReading'];

// Oracle capture-totality: /v1/oracle/streams omits `raw:<symbol>` rows
// (symbols the oracle publishes that map to no canonical asset) unless the
// caller opts in. This page is the intended opt-in — totality is the point
// of the explorer — but unmapped rows are reference-only, so they render
// in their own "Unmapped feeds" section and never join the mapped list.
const STREAMS_PARAMS = { include_unmapped: 'true' } as const;

// The `mapped` flag is the contract; the `raw:` wire prefix is the same
// fact read off the asset id, kept as a belt-and-braces guard so a row
// can never land in the mapped table by a missing flag alone.
function isUnmappedStream(s: OracleStream): boolean {
  return s.mapped === false || isRawOracleAsset(s.asset);
}

function SourceLink({ name }: { name: string }) {
  return (
    <Link
      href={hrefFor.source(name)}
      className="text-ink hover:text-brand-600 font-medium whitespace-nowrap"
    >
      {name}
    </Link>
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

function LoadingRow({ colSpan }: { colSpan: number }) {
  return (
    <TR className="hover:bg-transparent">
      <Td colSpan={colSpan}>
        <Skeleton className="h-4 w-64" />
      </Td>
    </TR>
  );
}

export function OraclesView() {
  const sources = useQuery<SourceRow[]>({
    queryKey: ['/v1/sources', 'stats', 'oracle'],
    queryFn: async () => {
      const env = await apiGet<{ data: SourceRow[] }>('/v1/sources', {
        include: 'stats',
      });
      const arr = env.data ?? [];
      return arr
        .filter((s) => s.class === 'oracle')
        .sort((a, b) => a.name.localeCompare(b.name));
    },
    refetchInterval: 60_000,
  });
  const streams = useQuery<OracleStream[]>({
    queryKey: ['/v1/oracle/streams', STREAMS_PARAMS],
    queryFn: async () => {
      const env = await apiGet<{ data: OracleStream[] }>(
        '/v1/oracle/streams',
        STREAMS_PARAMS,
      );
      return env.data ?? [];
    },
    refetchInterval: 60_000,
  });

  const oracles = sources.data ?? [];
  // Absent (fetch failed) ≠ empty (nothing registered / nothing observed).
  // Both tables below assert absence in their empty state; that claim is
  // only ours to make when the request actually answered.
  const registryAvailable = sources.data != null;
  const streamsAvailable = streams.data != null;

  const allStreamRows = useMemo(() => streams.data ?? [], [streams.data]);
  // Mapped rows drive the "Price streams" table and the per-oracle
  // activity counts; unmapped rows are listed separately, below.
  const streamRows = useMemo(
    () => allStreamRows.filter((s) => !isUnmappedStream(s)),
    [allStreamRows],
  );
  const unmappedRows = useMemo(
    () => allStreamRows.filter(isUnmappedStream),
    [allStreamRows],
  );
  // Per-oracle activity counts mapped and unmapped feeds alike: an oracle
  // publishing only unmapped symbols is live, not silent.
  const perSourceCounts = useMemo(() => {
    const map: Record<
      string,
      { mapped: number; unmapped: number; latestTs: string }
    > = {};
    for (const s of allStreamRows) {
      const cur = map[s.source] ?? { mapped: 0, unmapped: 0, latestTs: s.ts };
      if (isUnmappedStream(s)) cur.unmapped += 1;
      else cur.mapped += 1;
      if (s.ts > cur.latestTs) cur.latestTs = s.ts;
      map[s.source] = cur;
    }
    return map;
  }, [allStreamRows]);
  const [sourceFilter, setSourceFilter] = useState('all');
  const sourceOptions = useMemo(() => {
    const names = [...new Set(streamRows.map((s) => s.source))].sort();
    return [
      { label: 'All', value: 'all' },
      ...names.map((n) => ({ label: n, value: n })),
    ];
  }, [streamRows]);
  const activeFilter = sourceOptions.some((o) => o.value === sourceFilter)
    ? sourceFilter
    : 'all';
  const visibleStreamRows =
    activeFilter === 'all'
      ? streamRows
      : streamRows.filter((s) => s.source === activeFilter);

  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[{ label: 'Home', href: '/' }, { label: 'Oracles' }]}
        title="Oracles"
        meta={
          <Badge
            tone="neutral"
            title="Oracles are never included in the VWAP: mixing them would import their methodology and double-count the upstream markets they read."
          >
            VWAP weight 0
          </Badge>
        }
      />

      <Panel
        headingLevel={2}
        title="Connected oracles"
        hint="Reported for reference, never part of the VWAP"
        source={asExample('/v1/sources', { class: 'oracle', include: 'stats' })}
        bodyClassName="-mx-4 -mb-4"
      >
        <div className="overflow-x-auto">
          <Table>
            <THead>
              <tr>
                <Th>Oracle</Th>
                <Th align="right">Active streams</Th>
                <Th align="right">Last update</Th>
              </tr>
            </THead>
            <TBody>
              {sources.isLoading && <LoadingRow colSpan={3} />}
              {!sources.isLoading && !registryAvailable && (
                <NoteRow colSpan={3}>
                  Oracle registry unavailable right now — the sources read did
                  not return.
                  <Button
                    variant="secondary"
                    size="sm"
                    className="ml-3"
                    onClick={() => sources.refetch()}
                  >
                    Retry
                  </Button>
                </NoteRow>
              )}
              {!sources.isLoading &&
                registryAvailable &&
                oracles.length === 0 && (
                  <NoteRow colSpan={3}>No oracles registered.</NoteRow>
                )}
              {oracles.map((o) => {
                const perSrc = perSourceCounts[o.name];
                return (
                  <LinkRow href={hrefFor.source(o.name)} key={o.name}>
                    <Td>
                      <SourceLink name={o.name} />
                    </Td>
                    <Td align="right" className="font-mono whitespace-nowrap">
                      {perSrc && perSrc.mapped > 0 ? (
                        perSrc.mapped
                      ) : (
                        <span className="text-ink-faint">—</span>
                      )}
                      {perSrc && perSrc.unmapped > 0 && (
                        <span className="text-ink-muted ml-1.5 text-xs">
                          {perSrc.mapped > 0 ? '+' : ''}
                          {perSrc.unmapped} unmapped
                        </span>
                      )}
                    </Td>
                    <Td
                      align="right"
                      className="text-ink-muted font-mono text-xs"
                      title={perSrc?.latestTs}
                    >
                      {perSrc ? formatRelative(perSrc.latestTs) : '—'}
                    </Td>
                  </LinkRow>
                );
              })}
            </TBody>
          </Table>
        </div>
      </Panel>

      <Panel
        headingLevel={2}
        title={`Price streams${
          streamRows.length === 0
            ? ''
            : activeFilter === 'all'
              ? ` (${streamRows.length} active)`
              : ` (${visibleStreamRows.length} of ${streamRows.length})`
        }`}
        hint="Latest observation per oracle, asset and quote in the last 7 days"
        source={asExample('/v1/oracle/streams', STREAMS_PARAMS)}
        bodyClassName="-mx-4 -mb-4"
      >
        {sourceOptions.length > 2 && (
          <div className="mx-4 mb-4 overflow-x-auto">
            <Segmented
              ariaLabel="Oracle"
              options={sourceOptions}
              value={activeFilter}
              onChange={setSourceFilter}
            />
          </div>
        )}
        <div className="overflow-x-auto">
          <Table>
            <THead>
              <tr>
                <Th>Oracle</Th>
                <Th>Asset</Th>
                <Th className="hidden sm:table-cell">Quote</Th>
                <Th align="right">Latest price</Th>
                <Th align="right" className="hidden sm:table-cell">
                  Updated
                </Th>
              </tr>
            </THead>
            <TBody>
              {streams.isLoading && <LoadingRow colSpan={5} />}
              {!streams.isLoading && !streamsAvailable && (
                <NoteRow colSpan={5}>
                  Oracle observations unavailable right now — the streams read
                  did not return.
                  <Button
                    variant="secondary"
                    size="sm"
                    className="ml-3"
                    onClick={() => streams.refetch()}
                  >
                    Retry
                  </Button>
                </NoteRow>
              )}
              {!streams.isLoading &&
                streamsAvailable &&
                streamRows.length === 0 && (
                  <NoteRow colSpan={5}>
                    No oracle observations in the last 7 days.
                  </NoteRow>
                )}
              {visibleStreamRows.map((s, i) => (
                <TR key={`${s.source}|${s.asset}|${s.quote}|${i}`}>
                  <Td>
                    <SourceLink name={s.source} />
                  </Td>
                  <Td>
                    <AssetText canonical={s.asset} />
                  </Td>
                  <Td className="hidden sm:table-cell">
                    <AssetText canonical={s.quote} />
                  </Td>
                  <Td align="right" className="font-mono whitespace-nowrap">
                    {formatOraclePrice(s.price)}
                    <span className="text-ink-muted ml-1 text-xs sm:hidden">
                      {shortAssetText(s.quote)}
                    </span>
                  </Td>
                  <Td
                    align="right"
                    className="text-ink-muted hidden font-mono text-xs sm:table-cell"
                    title={s.ts}
                  >
                    {formatRelative(s.ts)}
                  </Td>
                </TR>
              ))}
            </TBody>
          </Table>
        </div>
      </Panel>

      {streamsAvailable && (
        <Panel
          headingLevel={2}
          title={`Unmapped feeds${unmappedRows.length > 0 ? ` (${unmappedRows.length})` : ''}`}
          hint="Symbols published under no canonical asset, recorded verbatim for reference only"
          source={asExample('/v1/oracle/streams', STREAMS_PARAMS)}
          bodyClassName="-mx-4 -mb-4"
        >
          <div className="overflow-x-auto">
            <Table>
              <THead>
                <tr>
                  <Th>Oracle</Th>
                  <Th>Raw symbol</Th>
                  <Th>Quote (assumed)</Th>
                  <Th align="right">Latest price</Th>
                  <Th align="right" className="hidden sm:table-cell">
                    Updated
                  </Th>
                </tr>
              </THead>
              <TBody>
                {unmappedRows.length === 0 && (
                  <NoteRow colSpan={5}>
                    No unmapped feeds in the last 7 days — every published
                    symbol maps to a canonical asset.
                  </NoteRow>
                )}
                {unmappedRows.map((s, i) => (
                  <TR key={`${s.source}|${s.asset}|${s.quote}|${i}`}>
                    <Td>
                      <SourceLink name={s.source} />
                    </Td>
                    <Td>
                      <span
                        className="text-ink font-mono text-xs"
                        title={`${s.asset} — unmapped oracle symbol`}
                      >
                        {isRawOracleAsset(s.asset)
                          ? rawOracleSymbol(s.asset)
                          : s.asset}
                      </span>
                    </Td>
                    <Td>
                      {/* Not a link, and labelled "assumed": for an unmapped
                          row the quote is the decoder's default (fiat:USD for
                          any symbol without a recognisable fiat suffix), not
                          an observed denomination. */}
                      <span
                        className="text-ink-muted font-mono text-xs"
                        title={`${s.quote} is the decoder's default for an unmapped symbol, not an observed denomination — this row's true quote is unknown`}
                      >
                        {s.quote}
                      </span>
                    </Td>
                    <Td align="right" className="font-mono">
                      {formatOraclePrice(s.price)}
                    </Td>
                    <Td
                      align="right"
                      className="text-ink-muted hidden font-mono text-xs sm:table-cell"
                      title={s.ts}
                    >
                      {formatRelative(s.ts)}
                    </Td>
                  </TR>
                ))}
              </TBody>
            </Table>
          </div>
        </Panel>
      )}

      <Panel
        headingLevel={2}
        title="SEP-40 compatibility"
        hint="Drop-in oracle interface: /v1/oracle/lastprice, /prices, /x_last_price"
        source={asExample('/v1/oracle/lastprice', { asset: 'native' })}
      >
        <p className="text-ink-body max-w-prose text-sm">
          The three endpoints match the SEP-40 contract trait. Routing your
          on-chain <code className="font-mono">lastprice()</code> calls through
          Stellar Index swaps in independent VWAP-backed prices without touching
          the calling contract.
        </p>
      </Panel>
    </Container>
  );
}
