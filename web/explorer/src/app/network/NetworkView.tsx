'use client';

import { useState } from 'react';
import dynamic from 'next/dynamic';
import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { Container,
  Breadcrumbs,
  EmptyState,
  Skeleton,
  Stat,
  StatCell,
  StatGrid,
  Table,
  TableWrap,
  TBody,
  Td,
  Th,
  THead,
  TR,
} from '@/components/ui';
import { usePools, useSources, isOnChainSource } from '@/api/hooks';
import type { NetworkStats } from '@/api/hooks';
import type { paths } from '@/api/types';
import { DonutChart } from '@/components/charts/DonutChart';
import { OperationMixPanel } from '@/components/NetworkInsight';
import { formatCompact } from '@/lib/format';
import {
  type Envelope,
  type Ledger,
  type LedgersPage,
  relativeAge,
  stroopsToXlm,
} from '../explorer-shared';

const LineChart = dynamic(
  () => import('@/components/charts/LineChart').then((m) => m.LineChart),
  { ssr: false, loading: () => <div className="h-[320px]" /> },
);

// Wire shapes derived from the generated OpenAPI contract
// (src/api/types.ts, `make web-generate-api`).
type ThroughputResp = NonNullable<
  paths['/network/throughput']['get']['responses'][200]['content']['application/json']['data']
>;



type Metric = 'ops' | 'txs' | 'events' | 'ledgers';
const METRICS: { key: Metric; label: string }[] = [
  { key: 'ops', label: 'Operations' },
  { key: 'txs', label: 'Transactions' },
  { key: 'events', label: 'Contract events' },
  { key: 'ledgers', label: 'Ledgers' },
];
const WINDOWS = [30, 90, 365];

export function NetworkView() {
  const [metric, setMetric] = useState<Metric>('ops');
  const [windowDays, setWindowDays] = useState(30);

  const statsQ = useQuery<NetworkStats>({
    queryKey: ['/v1/network/stats'],
    queryFn: async () => (await apiGet<Envelope<NetworkStats>>('/v1/network/stats', {})).data,
    staleTime: 30_000,
    refetchInterval: 60_000,
  });

  const ledgersQ = useQuery<LedgersPage>({
    queryKey: ['/v1/ledgers', 'network-strip'],
    queryFn: async () => (await apiGet<Envelope<LedgersPage>>('/v1/ledgers', { limit: 12 })).data,
    staleTime: 10_000,
  });

  const tpQ = useQuery<ThroughputResp>({
    queryKey: ['/v1/network/throughput', windowDays],
    queryFn: async () =>
      (await apiGet<Envelope<ThroughputResp>>('/v1/network/throughput', { window_days: windowDays })).data,
    staleTime: 60_000,
  });

  const buckets = tpQ.data?.buckets ?? [];
  // Today's bucket is still accumulating (`partial`) — the API contract
  // (GET /network/throughput) says to exclude it. Pre-fix the chart
  // plotted it anyway, ending every daily series on a phantom cliff;
  // drop it from BOTH the series and the headline total (same UXP-16
  // pattern as NetworkInsight).
  const completeBuckets = buckets.filter((b) => !b.partial);
  const points = completeBuckets.map((b) => ({
    time: Math.floor(Date.parse(`${b.day ?? ''}T00:00:00Z`) / 1000),
    value: b[metric] ?? 0,
  }));
  const total = completeBuckets.reduce((s, b) => s + (b[metric] ?? 0), 0);
  // Protocol-upgrade step markers: a marker on the day the end-of-day
  // protocol_version first differs from the previous complete day's.
  // Empty when the API doesn't serve protocol_version yet (no
  // fabricated upgrade dates) or no upgrade fell inside the window.
  // protocol_version is a uint32 with no omitempty, so it is never absent
  // on the wire — the real "unknown" sentinel is 0 (lake sentinel rows),
  // which must not mint phantom "protocol v0"/"vN" markers: both sides of
  // a step must be a real (>0) version.
  const upgradeMarkers = completeBuckets.flatMap((b, i) => {
    if (i === 0) return [];
    const prev = completeBuckets[i - 1].protocol_version;
    const cur = b.protocol_version;
    if (prev == null || cur == null || prev <= 0 || cur <= 0 || cur === prev) return [];
    return [{ time: Math.floor(Date.parse(`${b.day ?? ''}T00:00:00Z`) / 1000), label: `protocol v${cur}` }];
  });

  const s = statsQ.data;
  const tip = ledgersQ.data?.ledgers?.[0];

  return (
    <Container className="space-y-6 py-8">
      <header className="space-y-1">
        <Breadcrumbs items={[{ label: 'Home', href: '/' }, { label: 'Network' }]} />
        <h1 className="text-2xl font-semibold tracking-tight text-ink">Network</h1>
        <p className="max-w-2xl text-sm text-ink-muted">
          A live snapshot of the Stellar network — ledger chain state, throughput
          over time, what the network is doing right now, and the markets and
          sources feeding the lake.
        </p>
        {/* Nav revision 2026-08-24: the rail carries one Network entry;
            the chain sub-surfaces are reached from this hub. */}
        <nav aria-label="Network sub-surfaces" className="flex flex-wrap gap-2 pt-2">
          {[
            { href: '/network/ledgers', label: 'Ledgers' },
            { href: '/network/operations', label: 'Operations' },
            { href: '/transactions', label: 'Transactions' },
            { href: '/sources', label: 'Sources' },
          ].map((l) => (
            <Link
              key={l.href}
              href={l.href}
              className="rounded-lg border border-line bg-surface px-3 py-1 text-sm font-medium text-ink-body shadow-xs hover:border-brand-500 hover:text-brand-600"
            >
              {l.label} →
            </Link>
          ))}
        </nav>
      </header>

      <HeroStats stats={s} tip={tip} />

      <Panel
        title="Throughput"
        source={asExample('/v1/network/throughput', { window_days: windowDays })}
        bodyClassName="space-y-4"
      >
        <div className="flex flex-wrap items-center gap-2">
          <div className="flex gap-1">
            {METRICS.map((m) => (
              <button
                key={m.key}
                onClick={() => setMetric(m.key)}
                className={`rounded-md px-2.5 py-1 text-xs ${
                  metric === m.key
                    ? 'bg-brand-600 text-white'
                    : 'border border-line text-ink-body hover:border-brand-500'
                }`}
              >
                {m.label}
              </button>
            ))}
          </div>
          <div className="ml-auto flex gap-1">
            {WINDOWS.map((d) => (
              <button
                key={d}
                onClick={() => setWindowDays(d)}
                className={`rounded-md px-2.5 py-1 text-xs ${
                  windowDays === d
                    ? 'bg-surface-strong text-ink'
                    : 'border border-line text-ink-body hover:border-brand-500'
                }`}
              >
                {d}d
              </button>
            ))}
          </div>
        </div>

        {tpQ.isLoading && <Skeleton className="h-[320px] w-full" />}
        {tpQ.isError && <p className="text-sm text-ink-muted">Throughput is unavailable right now.</p>}
        {tpQ.data && points.length === 0 && (
          <p className="text-sm text-ink-muted">No ledgers in this window yet.</p>
        )}
        {points.length > 0 && (
          <>
            <p className="text-sm text-ink-body">
              <span className="font-mono tabular-nums">{formatCompact(total)}</span>{' '}
              {METRICS.find((m) => m.key === metric)?.label.toLowerCase()} over the last{' '}
              {completeBuckets.length} complete days
            </p>
            <LineChart
              data={points}
              height={320}
              positive
              markers={upgradeMarkers}
              ariaLabel={`Daily ${metric} on the Stellar network over the last ${windowDays} days${
                upgradeMarkers.length > 0 ? `, with ${upgradeMarkers.length} protocol upgrade marker(s)` : ''
              }`}
            />
            {upgradeMarkers.length > 0 && (
              <p className="text-xs text-ink-muted">
                Protocol upgrades in this window:{' '}
                {upgradeMarkers
                  .map((m) => `${m.label} on ${new Date(m.time * 1000).toISOString().slice(0, 10)}`)
                  .join(' · ')}
              </p>
            )}
          </>
        )}
      </Panel>

      <ChainEconomics buckets={completeBuckets} windowDays={windowDays} />

      <div className="grid gap-6 lg:grid-cols-2">
        <OperationMixPanel />
        <LatestLedgers ledgers={ledgersQ.data?.ledgers} loading={ledgersQ.isLoading} error={ledgersQ.isError} />
      </div>

      <div className="grid gap-6 lg:grid-cols-2">
        <TopMarkets />
        <ActiveSources />
      </div>

      <NetworkComposition />

      <DigDeeper />
    </Container>
  );
}

// ChainEconomics — the chain-state series that ride the same
// /v1/network/throughput scan: daily fee burn (the delta of the
// cumulative fee_pool between consecutive COMPLETE days — fee_pool
// only ever grows post-inflation-removal, so the delta is the XLM
// the network burned in fees that day) and total XLM in existence.
// Buckets are stroop strings past 2^53 (ADR-0003): divide as BigInt
// first, then convert the safely-small quotient/delta to a JS
// number for chart geometry only. Renders an honest note instead of
// charts when the API doesn't serve the fields yet.
function ChainEconomics({
  buckets,
  windowDays,
}: {
  buckets: ThroughputResp['buckets'];
  windowDays: number;
}) {
  const rows = buckets ?? [];
  const isStroops = (s: string | undefined): s is string => !!s && /^\d+$/.test(s);
  const dayEpoch = (day: string | undefined) => Math.floor(Date.parse(`${day ?? ''}T00:00:00Z`) / 1000);

  const served = rows.some((b) => isStroops(b.fee_pool) || isStroops(b.total_coins));

  const feeBurn: { time: number; value: number }[] = [];
  for (let i = 1; i < rows.length; i++) {
    const prev = rows[i - 1].fee_pool;
    const cur = rows[i].fee_pool;
    if (!isStroops(prev) || !isStroops(cur)) continue;
    // Delta in stroops is small (daily fees) — exact in a JS number.
    feeBurn.push({ time: dayEpoch(rows[i].day), value: Number(BigInt(cur) - BigInt(prev)) / 1e7 });
  }

  const totalCoins = rows
    .filter((b) => isStroops(b.total_coins))
    .map((b) => ({
      time: dayEpoch(b.day),
      // BigInt divide to XLM first (truncates sub-XLM), then the ~5e10
      // quotient is float-safe.
      value: Number(BigInt(b.total_coins as string) / 10_000_000n),
    }));

  return (
    <div className="grid gap-6 lg:grid-cols-2">
      <Panel
        title="Daily fee burn"
        hint="XLM paid in transaction fees per complete UTC day — the day-over-day delta of the cumulative network fee pool, off each day's last ledger."
        source={asExample('/v1/network/throughput', { window_days: windowDays })}
      >
        {!served && (
          <p className="text-sm text-ink-muted">
            Chain-state series unavailable — the API isn&apos;t serving fee-pool buckets yet.
          </p>
        )}
        {served && feeBurn.length === 0 && (
          <p className="text-sm text-ink-muted">Not enough complete days in this window to compute a delta.</p>
        )}
        {feeBurn.length > 0 && (
          <LineChart
            data={feeBurn}
            height={220}
            positive
            legend={{
              valueLabel: 'XLM burned',
              formatValue: (n) => `${n.toLocaleString(undefined, { maximumFractionDigits: 0 })} XLM`,
            }}
            ariaLabel={`Daily XLM fee burn over the last ${windowDays} days`}
          />
        )}
      </Panel>
      <Panel
        title="Total XLM"
        hint="Total lumens in existence at each day's last ledger. Flat is the expected shape — XLM has no ongoing issuance."
        source={asExample('/v1/network/throughput', { window_days: windowDays })}
      >
        {!served && (
          <p className="text-sm text-ink-muted">
            Chain-state series unavailable — the API isn&apos;t serving total-coins buckets yet.
          </p>
        )}
        {served && totalCoins.length === 0 && (
          <p className="text-sm text-ink-muted">No complete days in this window.</p>
        )}
        {totalCoins.length > 0 && (
          <LineChart
            data={totalCoins}
            height={220}
            positive
            area={false}
            legend={{
              valueLabel: 'Total XLM',
              formatValue: (n) => `${formatCompact(n)} XLM`,
            }}
            ariaLabel={`Total XLM in existence over the last ${windowDays} days`}
          />
        )}
      </Panel>
    </div>
  );
}

// HeroStats — the network at-a-glance, blending the aggregate
// /v1/network/stats snapshot with the chain-state fields off the
// freshest ledger header (total XLM, fee pool, protocol version).
function HeroStats({ stats: s, tip }: { stats?: NetworkStats; tip?: Ledger }) {
  // Stellar ON-CHAIN 24h volume (SDEX + Soroban DEXes), summed from the
  // DEX-subclass /v1/sources. /v1/network/stats.volume_24h_usd is the
  // ALL-source total (CEX feeds dominate) — not "Stellar volume".
  // Matches the home strip. (The "Volume by venue type" donut below
  // still shows the full CEX-vs-on-chain split by design.)
  const { data: sources } = useSources(undefined, true);
  const onChain = (sources ?? []).filter(isOnChainSource);
  const stellarVolume = onChain
    .filter((x) => x.subclass === 'dex')
    .reduce((sum, x) => sum + (x.volume_24h_usd ? Number(x.volume_24h_usd) : 0), 0);
  // Stellar markets = active (venue, pair) pools summed across on-chain
  // DEX venues (a pair traded on two DEXes counts as two markets).
  const stellarMarkets = onChain.reduce((sum, x) => sum + (x.markets_count_24h ?? 0), 0);
  const stellarSources = onChain.length;
  return (
    <StatGrid cols={4}>
      <StatCell>
        <Stat
          label="Latest ledger"
          value={s ? `#${s.latest_ledger.toLocaleString('en-US')}` : '—'}
          sub={tip ? `${relativeAge(tip.close_time)} · protocol v${tip.protocol_version}` : undefined}
        />
      </StatCell>
      <StatCell>
        <Stat
          label="24h volume"
          value={stellarVolume > 0 ? `$${formatCompact(stellarVolume)}` : '—'}
          sub="Stellar on-chain"
        />
      </StatCell>
      <StatCell>
        <Stat
          label="Stellar markets (24h)"
          value={stellarMarkets > 0 ? formatCompact(stellarMarkets) : '—'}
          sub="across DEX venues"
        />
      </StatCell>
      <StatCell>
        <Stat label="Assets indexed" value={s ? formatCompact(s.assets_indexed) : '—'} />
      </StatCell>
      <StatCell>
        <Stat
          label="Total XLM"
          value={tip?.total_coins ? xlmCompact(tip.total_coins) : '—'}
          sub={tip?.fee_pool ? `${xlmCompact(tip.fee_pool)} XLM in fee pool` : undefined}
        />
      </StatCell>
      <StatCell>
        <Stat
          label="Base fee"
          value={tip?.base_fee != null ? `${tip.base_fee.toLocaleString('en-US')}` : '—'}
          sub="stroops / op"
        />
      </StatCell>
      <StatCell>
        <Stat
          label="Stellar sources"
          value={stellarSources > 0 ? String(stellarSources) : '—'}
          sub="on-chain venues"
        />
      </StatCell>
      <StatCell>
        <Stat
          label="Network"
          value="Pubnet"
          sub={tip ? `${formatCompact(tip.tx_count ?? 0)} tx · ${formatCompact(tip.op_count ?? 0)} ops last ledger` : 'mainnet'}
        />
      </StatCell>
    </StatGrid>
  );
}

// OperationMix — proportional bars of operation types over the
// trailing ~24h, straight from the /v1/operations type aggregate.
// Far more legible than the raw chip cloud on /operations.
// LatestLedgers — the chain tip, newest first. Each row deep-links
// to the per-ledger explorer page.
function LatestLedgers({
  ledgers,
  loading,
  error,
}: {
  ledgers?: Ledger[];
  loading: boolean;
  error: boolean;
}) {
  const rows = (ledgers ?? []).slice(0, 12);
  return (
    <Panel
      title="Latest ledgers"
      source={asExample('/v1/ledgers', { limit: 12 })}
      bodyClassName="-mx-4 -mb-4"
    >
      {loading && <div className="px-4 pb-4"><Skeleton className="h-40 w-full" /></div>}
      {error && <p className="px-4 pb-4 text-sm text-ink-muted">Ledgers are unavailable right now.</p>}
      {!loading && !error && rows.length === 0 && (
        <div className="px-4 pb-4"><EmptyState title="No ledgers yet." /></div>
      )}
      {rows.length > 0 && (
        <TableWrap className="rounded-none border-0">
          <Table>
            <THead>
              <TR className="hover:bg-transparent">
                <Th>Ledger</Th>
                <Th align="right">Txs</Th>
                <Th align="right">Ops</Th>
                <Th align="right">Events</Th>
                <Th align="right">Age</Th>
              </TR>
            </THead>
            <TBody>
              {rows.map((l) => (
                <TR key={l.sequence}>
                  <Td>
                    <Link
                      href={`/ledgers/${l.sequence}/`}
                      className="font-mono tabular-nums text-brand-600 hover:underline"
                    >
                      #{(l.sequence ?? 0).toLocaleString('en-US')}
                    </Link>
                  </Td>
                  <Td align="right">{(l.tx_count ?? 0).toLocaleString('en-US')}</Td>
                  <Td align="right">{(l.op_count ?? 0).toLocaleString('en-US')}</Td>
                  <Td align="right">{(l.soroban_event_count ?? 0).toLocaleString('en-US')}</Td>
                  <Td align="right" className="text-ink-muted">{relativeAge(l.close_time)}</Td>
                </TR>
              ))}
            </TBody>
          </Table>
        </TableWrap>
      )}
      <div className="px-4 pt-3 text-xs">
        <Link href="/ledgers" className="text-brand-600 hover:underline">
          All ledgers →
        </Link>
      </div>
    </Panel>
  );
}

// TopMarkets — top Stellar on-chain pools by trailing-24h USD volume.
// Sourced from /v1/pools (DEX-subclass only, server-side scoped), NOT
// /v1/markets — the latter aggregates across off-chain CEX reference
// feeds (BTC/USDT etc.) that aren't Stellar markets at all.
function TopMarkets() {
  const { data, isLoading, isError } = usePools(8, 'volume_24h_usd_desc');
  const rows = (data ?? []).slice(0, 8);
  return (
    <Panel
      title="Top Stellar markets"
      hint="On-chain DEX pools by trailing-24h volume — SDEX + Soroban DEXes only."
      source={asExample('/v1/pools', { limit: 8, order_by: 'volume_24h_usd_desc' })}
      bodyClassName="-mx-4 -mb-4"
    >
      {isLoading && <div className="px-4 pb-4"><Skeleton className="h-40 w-full" /></div>}
      {isError && <p className="px-4 pb-4 text-sm text-ink-muted">Markets are unavailable right now.</p>}
      {!isLoading && !isError && rows.length === 0 && (
        <div className="px-4 pb-4"><EmptyState title="No Stellar markets returned." /></div>
      )}
      {rows.length > 0 && (
        <TableWrap className="rounded-none border-0">
          <Table>
            <THead>
              <TR className="hover:bg-transparent">
                <Th>Pair</Th>
                <Th>Venue</Th>
                <Th align="right">24h volume</Th>
              </TR>
            </THead>
            <TBody>
              {rows.map((m) => {
                const slug = `${m.base}~${m.quote}`;
                return (
                  <TR key={`${m.source}:${slug}`}>
                    <Td>
                      <Link
                        href={`/markets/${encodeURIComponent(slug)}`}
                        className="font-medium text-ink hover:text-brand-600"
                      >
                        {shortAsset(m.base)}
                        <span className="mx-1 text-ink-faint">/</span>
                        {shortAsset(m.quote)}
                      </Link>
                    </Td>
                    <Td>
                      <Link
                        href={`/dexes/${encodeURIComponent(m.source)}`}
                        className="text-ink-muted hover:text-brand-600"
                      >
                        {m.source}
                      </Link>
                    </Td>
                    <Td align="right" className="font-mono">
                      {m.volume_24h_usd ? `$${formatCompact(Number(m.volume_24h_usd))}` : '—'}
                    </Td>
                  </TR>
                );
              })}
            </TBody>
          </Table>
        </TableWrap>
      )}
      <div className="px-4 pt-3 text-xs">
        <Link href="/dexes" className="text-brand-600 hover:underline">
          All DEX pools →
        </Link>
      </div>
    </Panel>
  );
}

// ActiveSources — Stellar on-chain sources ranked by trailing-24h USD
// volume. Off-chain reference feeds (CEX / FX / aggregators) are
// excluded — they dominate raw volume but aren't Stellar activity;
// see /exchanges for those.
function ActiveSources() {
  const { data, isLoading, isError } = useSources(undefined, true);
  const rows = [...(data ?? [])]
    .filter(isOnChainSource)
    .sort((a, b) => Number(b.volume_24h_usd ?? 0) - Number(a.volume_24h_usd ?? 0))
    .slice(0, 8);
  return (
    <Panel
      title="Most active Stellar sources"
      hint="On-chain venues only — CEX / aggregator / FX feeds live on /exchanges."
      source={asExample('/v1/sources', { include: 'stats' })}
      bodyClassName="-mx-4 -mb-4"
    >
      {isLoading && <div className="px-4 pb-4"><Skeleton className="h-40 w-full" /></div>}
      {isError && <p className="px-4 pb-4 text-sm text-ink-muted">Sources are unavailable right now.</p>}
      {!isLoading && !isError && rows.length === 0 && (
        <div className="px-4 pb-4"><EmptyState title="No sources reporting volume." /></div>
      )}
      {rows.length > 0 && (
        <TableWrap className="rounded-none border-0">
          <Table>
            <THead>
              <TR className="hover:bg-transparent">
                <Th>Source</Th>
                <Th>Class</Th>
                <Th align="right">24h volume</Th>
              </TR>
            </THead>
            <TBody>
              {rows.map((src) => (
                <TR key={src.name}>
                  <Td>
                    <Link
                      href={`/sources/${encodeURIComponent(src.name)}`}
                      className="font-medium text-ink hover:text-brand-600"
                    >
                      {src.name}
                    </Link>
                  </Td>
                  <Td className="text-ink-muted">{src.class}</Td>
                  <Td align="right" className="font-mono">
                    {src.volume_24h_usd ? `$${formatCompact(Number(src.volume_24h_usd))}` : '—'}
                  </Td>
                </TR>
              ))}
            </TBody>
          </Table>
        </TableWrap>
      )}
      <div className="px-4 pt-3 text-xs">
        <Link href="/sources" className="text-brand-600 hover:underline">
          All sources →
        </Link>
      </div>
    </Panel>
  );
}

// NetworkComposition — trailing-24h on-chain USD volume split by
// Stellar venue (sdex / aquarius / phoenix / soroswap / comet).
// Off-chain feeds are excluded (this is the Stellar page); with CEX
// gone the old venue-TYPE split collapses to a single "On-chain DEX"
// slice, so we break down by venue NAME instead — the useful cut.
// Derived from the same /v1/sources?include=stats the directory uses.
function NetworkComposition() {
  const { data, isLoading, isError } = useSources(undefined, true);
  // `?include=stats` soft-fails server-side ("serve the registry without
  // stats") and volume_24h_usd is omitempty — on that degrade every row
  // arrives without it. Distinguish "stats join absent" from a genuine
  // zero-volume day so we never claim "no on-chain volume" off missing
  // data.
  const statsAvailable = (data ?? []).some((s) => s.volume_24h_usd != null);
  const slices = (data ?? [])
    .filter(isOnChainSource)
    .map((s) => ({ label: s.name, value: Number(s.volume_24h_usd ?? 0) }))
    .filter((x) => Number.isFinite(x.value) && x.value > 0)
    .sort((a, b) => b.value - a.value);
  const total = slices.reduce((sum, s) => sum + s.value, 0);

  return (
    <Panel
      title="Volume by Stellar venue — 24h"
      hint="Share of trailing-24h on-chain USD volume across Stellar DEX venues."
      source={asExample('/v1/sources', { include: 'stats' })}
    >
      {isLoading && <Skeleton className="h-40 w-full" />}
      {isError && <p className="text-sm text-ink-muted">Composition is unavailable right now.</p>}
      {!isLoading && !isError && !statsAvailable && (
        <p className="text-sm text-ink-muted">
          Volume stats unavailable — refreshing.
        </p>
      )}
      {!isLoading && !isError && statsAvailable && slices.length === 0 && (
        <EmptyState title="No on-chain volume in the last 24h." />
      )}
      {slices.length > 0 && (
        <DonutChart
          data={slices}
          centerLabel={`$${formatCompact(total)}`}
          centerSub="24h vol"
          formatValue={(n) => `$${formatCompact(n)}`}
        />
      )}
    </Panel>
  );
}

// DigDeeper — a navigation grid into the analytical surfaces that
// hang off the network: the explorer entities + the price-quality
// feeds. Makes the cross-page structure discoverable rather than
// buried in the header nav.
const DEEPER: { href: string; title: string; blurb: string }[] = [
  { href: '/operations', title: 'Operations', blurb: 'Every decoded op, newest first' },
  { href: '/transactions', title: 'Transactions', blurb: 'Recent transactions across the chain' },
  { href: '/ledgers', title: 'Ledgers', blurb: 'The full ledger chain to genesis' },
  { href: '/accounts', title: 'Accounts', blurb: 'Account directory + activity' },
  { href: '/contracts', title: 'Contracts', blurb: 'Soroban contracts + their events' },
  { href: '/protocols', title: 'Protocols', blurb: 'Per-protocol verified coverage' },
  { href: '/dexes', title: 'DEXes', blurb: 'On-chain pools + AMMs' },
  { href: '/lending', title: 'Lending', blurb: 'Blend pools — TVL, utilization, APY' },
  { href: '/oracles', title: 'Oracles', blurb: 'Reflector / Band / Redstone feeds' },
  { href: '/mev', title: 'MEV', blurb: 'Detected arbitrage cycles' },
  { href: '/anomalies', title: 'Anomalies', blurb: 'Price-freeze + outlier events' },
  { href: '/divergences', title: 'Divergences', blurb: 'Cross-checks vs reference venues' },
];

function DigDeeper() {
  return (
    <Panel title="Dig deeper" bodyClassName="grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-4">
      {DEEPER.map((d) => (
        <Link
          key={d.href}
          href={d.href}
          className="group rounded-card border border-line bg-surface p-3 transition-colors hover:border-brand-500 hover:bg-surface-muted"
        >
          <div className="text-sm font-medium text-ink group-hover:text-brand-600">{d.title}</div>
          <div className="mt-0.5 text-xs text-ink-muted">{d.blurb}</div>
        </Link>
      ))}
    </Panel>
  );
}

// xlmCompact renders a stroop integer string as a compact XLM figure
// (e.g. "50.5B"). total_coins is ~117× past 2^53, so divide as BigInt
// first (truncating sub-XLM), then the quotient (~5e10) is safely
// inside the float range for compact display. (ADR-0003.)
function xlmCompact(stroops: string): string {
  const t = stroops.trim();
  if (!/^-?\d+$/.test(t)) return stroopsToXlm(stroops);
  return formatCompact(Number(BigInt(t) / 10_000_000n));
}

function shortAsset(canonical: string | undefined | null): string {
  if (!canonical) return '—';
  if (canonical === 'native') return 'XLM';
  if (canonical.startsWith('fiat:')) return canonical.replace('fiat:', '');
  if (canonical.startsWith('crypto:')) return canonical.replace('crypto:', '');
  const dashIx = canonical.indexOf('-');
  if (dashIx === -1) return canonical;
  return canonical.slice(0, dashIx);
}
