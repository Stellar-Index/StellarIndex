'use client';

import Link from 'next/link';
import { hrefFor } from '@/lib/hrefFor';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import {
  formatCompact,
  formatCompactUnits,
  ratioPct,
  sumDecimalStrings,
} from '@/lib/format';
import { sourceToneClass } from '@/lib/pillTone';
import { SourceSparkline } from '@/components/SourceSparkline';
import {
  Container,
  PageHeader,
  Stat,
  StatCell,
  StatGrid,
  TBody,
  TR,
  Table,
  Td,
  Th,
  THead,
} from '@/components/ui';

// /v1/sources rows from the generated OpenAPI contract, via the shared
// alias in src/api/hooks.ts.
import type { Source as SourceRow } from '@/api/hooks';

const LABEL: Record<string, string> = {
  binance: 'Binance',
  coinbase: 'Coinbase',
  kraken: 'Kraken',
  bitstamp: 'Bitstamp',
};

export function ExchangesView() {
  const q = useQuery<SourceRow[]>({
    queryKey: ['/v1/sources', 'stats,sparkline', 'cex'],
    queryFn: async () => {
      const env = await apiGet<{ data: SourceRow[] }>('/v1/sources', {
        include: 'stats,sparkline',
      });
      const arr = env.data ?? [];
      return arr
        .filter((s) => s.class === 'exchange' && s.subclass === 'cex')
        .sort((a, b) => {
          const av = a.volume_24h_usd ? Number(a.volume_24h_usd) : 0;
          const bv = b.volume_24h_usd ? Number(b.volume_24h_usd) : 0;
          return bv - av;
        });
    },
  });

  // Absent (registry fetch failed) ≠ empty (no CEX registered): keep the
  // undefined so the table renders "unavailable" instead of the claim
  // "No CEX sources reporting."
  const rows = q.data ?? [];
  const registryAvailable = q.data != null;
  // `?include=stats` soft-fails server-side (internal/api/v1/sources.go:
  // "serve the registry without stats") and every stats column is
  // omitempty — on that degrade EVERY row arrives bare. Detect the
  // wholesale absence so the page renders '—', not a fabricated
  // "$0 across 0 trades" claim. When any row carries stats, a bare field
  // on another row is a genuine present-and-zero (omitempty drops 0).
  const statsAvailable = rows.some(
    (r) =>
      r.volume_24h_usd != null ||
      r.trade_count_24h != null ||
      r.markets_count_24h != null,
  );
  const totalVol = sumDecimalStrings(rows.map((r) => r.volume_24h_usd)) ?? '0';
  const totalTrades = rows.reduce((s, r) => s + (r.trade_count_24h ?? 0), 0);
  const totalMarkets = rows.reduce((s, r) => s + (r.markets_count_24h ?? 0), 0);

  return (
    <Container className="space-y-8 py-8 sm:py-10">
      <PageHeader
        eyebrow="Centralised venues"
        title="Exchanges"
        description={
          <>
            Connected centralised exchanges feeding the Stellar Index
            aggregator. Per-venue 24h USD volume, trade count, and coverage.
            Click a venue for its full pair list. On-chain DEXes and AMM pools
            live at{' '}
            <Link href="/dexes" className="text-brand-600 hover:underline">
              /dexes
            </Link>
            .
          </>
        }
      />

      {rows.length > 0 && (
        <StatGrid cols={3}>
          <StatCell>
            <Stat
              label="24h volume"
              value={statsAvailable ? `$${formatCompactUnits(totalVol)}` : '—'}
            />
          </StatCell>
          <StatCell>
            <Stat
              label="24h trades"
              value={statsAvailable ? formatCompact(totalTrades) : '—'}
            />
          </StatCell>
          <StatCell>
            <Stat
              label="Pairs covered"
              value={
                statsAvailable ? totalMarkets.toLocaleString('en-US') : '—'
              }
            />
          </StatCell>
        </StatGrid>
      )}

      <Panel
        headingLevel={2}
        title={
          registryAvailable
            ? `${rows.length} centralised exchanges`
            : 'Centralised exchanges'
        }
        hint={
          rows.length > 0 && statsAvailable
            ? `Total 24h: $${formatCompactUnits(totalVol)} across ${formatCompact(totalTrades)} trades on ${totalMarkets} pairs`
            : rows.length > 0
              ? '24h stats unavailable — refreshing'
              : 'Source: /v1/sources?include=stats'
        }
        source={asExample('/v1/sources', { include: 'stats' })}
        bodyClassName="-mx-4"
      >
        <div className="overflow-x-auto">
          <Table>
            <THead>
              <tr>
                <Th>#</Th>
                <Th>Exchange</Th>
                <Th align="right">24h volume</Th>
                <Th>24h chart</Th>
                <Th align="right">24h trades</Th>
                <Th align="right">Pairs</Th>
                <Th align="right">Share of CEX vol</Th>
              </tr>
            </THead>
            <TBody>
              {q.isLoading && (
                <tr>
                  <td
                    colSpan={7}
                    className="text-ink-muted px-4 py-6 text-center text-sm"
                  >
                    Loading exchanges…
                  </td>
                </tr>
              )}
              {!q.isLoading && !registryAvailable && (
                <tr>
                  <td
                    colSpan={7}
                    className="text-ink-muted px-4 py-6 text-center text-sm"
                  >
                    Exchange registry unavailable right now — retry shortly.
                  </td>
                </tr>
              )}
              {!q.isLoading && registryAvailable && rows.length === 0 && (
                <tr>
                  <td
                    colSpan={7}
                    className="text-ink-muted px-4 py-6 text-center text-sm"
                  >
                    No CEX sources reporting.
                  </td>
                </tr>
              )}
              {rows.map((r, i) => {
                const vol = r.volume_24h_usd ? Number(r.volume_24h_usd) : 0;
                const tone = sourceToneClass(r.name);
                const label = LABEL[r.name] ?? r.name;
                const share = ratioPct(r.volume_24h_usd, totalVol) ?? 0;
                return (
                  <TR key={r.name}>
                    <Td>
                      <span className="text-ink-faint font-mono text-[11px]">
                        {i + 1}
                      </span>
                    </Td>
                    <Td>
                      <Link
                        href={hrefFor.exchange(r.name)}
                        className={`inline-block rounded-sm px-1.5 py-0.5 text-[11px] font-medium tracking-wider uppercase hover:underline ${tone}`}
                      >
                        {label}
                      </Link>
                    </Td>
                    <Td align="right">
                      {vol > 0 ? (
                        <span className="font-mono tabular-nums">
                          ${formatCompact(vol)}
                        </span>
                      ) : (
                        <span className="text-ink-faint">—</span>
                      )}
                    </Td>
                    <Td>
                      <SourceSparkline buckets={r.volume_history_24h} />
                    </Td>
                    <Td align="right">
                      <span className="text-ink-body font-mono tabular-nums">
                        {r.trade_count_24h && r.trade_count_24h > 0
                          ? formatCompact(r.trade_count_24h)
                          : statsAvailable
                            ? '0'
                            : '—'}
                      </span>
                    </Td>
                    <Td align="right">
                      <span className="text-ink-body font-mono tabular-nums">
                        {statsAvailable ? (r.markets_count_24h ?? 0) : '—'}
                      </span>
                    </Td>
                    <Td align="right">
                      <div className="inline-flex items-center gap-2">
                        <div className="bg-line h-1.5 w-16 overflow-hidden rounded-full">
                          <div
                            className="bg-brand-500 h-full"
                            style={{ width: `${Math.min(100, share)}%` }}
                          />
                        </div>
                        <span className="text-ink-muted font-mono text-xs tabular-nums">
                          {share.toFixed(1)}%
                        </span>
                      </div>
                    </Td>
                  </TR>
                );
              })}
            </TBody>
          </Table>
        </div>
      </Panel>

      <p className="text-ink-muted text-xs">
        Sources are pulled from the static venue registry; per-venue 24h
        activity is aggregated from{' '}
        <code className="font-mono text-[11px]">trades</code> in TimescaleDB. We
        deliberately subscribe to a curated set of pairs per venue (the
        top-liquidity XLM markets and the crypto anchors that triangulate into
        them). Venue prices appear only blended with the other sources on the
        market pages, never as one venue&apos;s feed alone.
      </p>
    </Container>
  );
}
