'use client';

import Link from 'next/link';

import { useMemo } from 'react';

import { Panel } from '@/components/reveal';
import { asExample } from '@/api/client';
import { SourceSparkline } from '@/components/SourceSparkline';
import { useMarkets, type Market } from '@/api/hooks';
import { useLedgerFollow } from '@/lib/live/hooks';
import {
  formatCompact,
  formatCompactUnits,
  formatRelative,
  divideDecimalString,
} from '@/lib/format';

/**
 * MarketsTabPanel — backs the "Markets" tab on /assets/[slug].
 *
 * Pulls `/v1/markets?asset=<slug>` and renders the result. The
 * server expands catalogue slugs (e.g. "btc", "usdc", "xlm") to
 * every asset_id form the catalogue knows: Stellar networks[]
 * .asset_id entries plus the global `crypto:<TICKER>` /
 * `fiat:<TICKER>` form, then unions trade rows where any of those
 * appear on either side of a pair. Net result: a single panel
 * surfaces both Stellar SDEX markets (USDC-GA5Z..., native, …)
 * AND CEX markets (crypto:BTC/crypto:USDT, crypto:XLM/fiat:USD,
 * etc.) under the same slug — the cross-chain summary view per
 * the assets-redesign spec.
 *
 * For non-catalogue slugs (long-tail classic_assets), the server
 * treats `?asset=` as a canonical asset_id and returns the pre-
 * existing per-asset_id stream.
 */
export function MarketsTabPanel({ assetID }: { assetID: string }) {
  // sparkline: per-row 24h hourly USD-volume buckets (?include=sparkline)
  // for the chart column; rows the flag returns nothing for render "—".
  const markets = useMarkets(100, 'volume_24h_usd_desc', {
    asset: assetID,
    sparkline: true,
  });
  useLedgerFollow(['/v1/markets']);

  // Sort client-side by trade_count_24h desc as a secondary order
  // — the API returns the fanned-out merge already sorted by
  // trade_count, but a defensive re-sort keeps the column order
  // predictable when the API path is unfilteredly volume-desc.
  const matched = useMemo(() => {
    if (!markets.data) return [];
    return [...markets.data.markets].sort(
      (a, b) => b.trade_count_24h - a.trade_count_24h,
    );
  }, [markets.data]);

  const barMax = useMemo(() => maxVolume(matched), [matched]);

  // A next cursor means the server holds more rows than this page: the count
  // is a lower bound, never a total.
  const truncated = Boolean(markets.data?.nextCursor);

  if (markets.isError) {
    return (
      <Panel
        headingLevel={2}
        title="Markets"
        source={asExample('/v1/markets', { limit: 100 })}
        bodyClassName="text-sm text-down-strong"
      >
        Failed to load markets.
      </Panel>
    );
  }
  if (markets.isLoading) {
    return (
      <Panel
        headingLevel={2}
        title="Markets"
        source={asExample('/v1/markets', { limit: 100 })}
        bodyClassName="text-sm text-ink-muted"
      >
        Loading…
      </Panel>
    );
  }
  if (matched.length === 0) {
    return (
      <Panel
        headingLevel={2}
        title="Markets"
        hint="No active markets in the last 14 days"
        source={asExample('/v1/markets', { limit: 100 })}
        bodyClassName="text-sm text-ink-muted"
      >
        No (base, quote) pair involving this asset has traded in the recency
        window.
      </Panel>
    );
  }

  return (
    <Panel
      headingLevel={2}
      title={
        truncated
          ? `Top ${matched.length} markets by 24h volume`
          : `${matched.length} active market${matched.length === 1 ? '' : 's'}`
      }
      hint={
        truncated
          ? 'More pairs exist than are listed here; pairs that traded in the last 14 days'
          : 'Pairs involving this coin that traded in the last 14 days'
      }
      source={asExample('/v1/markets', { limit: 100 })}
      bodyClassName="-mx-4"
    >
      <div className="overflow-x-auto">
        <table className="divide-line min-w-full divide-y text-sm">
          <thead>
            <tr className="text-ink-muted text-left text-[11px] tracking-wider uppercase">
              <Th>Side</Th>
              <Th>Pair</Th>
              <Th align="right">24h volume</Th>
              <Th align="right">24h trades</Th>
              <Th align="right">Avg trade</Th>
              <Th>24h chart</Th>
              <Th align="right">Last trade</Th>
            </tr>
          </thead>
          <tbody className="divide-line-subtle divide-y">
            {matched.map((m) => (
              <Row
                key={`${m.base}|${m.quote}`}
                m={m}
                assetID={assetID}
                barMax={barMax}
              />
            ))}
          </tbody>
        </table>
      </div>
    </Panel>
  );
}

/** Largest listed 24h USD volume; bar geometry only, never a displayed number. */
export function maxVolume(
  markets: readonly { volume_24h_usd?: string | null }[],
): number {
  return Math.max(
    0,
    ...markets.map((m) => Number(m.volume_24h_usd)).filter(Number.isFinite),
  );
}

function Row({
  m,
  assetID,
  barMax,
}: {
  m: Market;
  assetID: string;
  barMax: number;
}) {
  const vol = Number(m.volume_24h_usd);
  const barPct =
    barMax > 0 && Number.isFinite(vol) && vol > 0 ? (vol / barMax) * 100 : 0;
  // The server expands a catalogue slug ("usdc") into its
  // asset_ids, so strict equality against the slug never matched and
  // rows where the asset IS the base rendered "quote · vs itself".
  // Match by expanded-form prefix: the asset's code appears at the
  // start of its expanded ids ("USDC-GA5Z…"), CEX/FX ids carry it after
  // a crypto:/fiat: namespace, and "native"/"XLM" alias each other.
  const want = sideTicker(assetID);
  const matches = (side: string) => {
    if (side === assetID) return true;
    const got = sideTicker(side);
    return (
      got === want || got.startsWith(`${want}-`) || got.startsWith(`${want}:`)
    );
  };
  const isBase = matches(m.base ?? '');
  const counterparty = isBase ? m.quote : m.base;
  const pairSlug = `${m.base}~${m.quote}`;
  const avgTrade = divideDecimalString(m.volume_24h_usd, m.trade_count_24h);
  return (
    <tr className="hover:bg-surface-muted">
      <Td>
        <span className="bg-surface-subtle text-ink-body rounded-sm px-1.5 py-0.5 text-[10px] tracking-wider uppercase">
          {isBase ? 'base' : 'quote'}
        </span>
      </Td>
      <Td>
        {/* AM-12: rows link their pair page instead of dead-ending. */}
        <Link
          href={`/markets/${encodeURIComponent(pairSlug)}/`}
          className="hover:text-brand-600"
        >
          <span className="font-medium">vs </span>
          <span className="font-mono text-xs">
            {shortAsset(counterparty ?? '')}
          </span>
        </Link>
      </Td>
      <Td align="right">
        <span className="font-mono text-xs tabular-nums">
          {m.volume_24h_usd ? `$${formatCompactUnits(m.volume_24h_usd)}` : '—'}
        </span>
        {barPct > 0 && (
          <span
            aria-hidden
            className="bg-surface-muted mt-1 ml-auto block h-1 w-24 overflow-hidden rounded-full"
          >
            <span
              className="bg-brand-500 block h-full rounded-full"
              style={{ width: `${Math.max(barPct, 2)}%`, marginLeft: 'auto' }}
            />
          </span>
        )}
      </Td>
      <Td align="right">
        <span className="font-mono tabular-nums">
          {formatCompact(m.trade_count_24h)}
        </span>
      </Td>
      <Td align="right">
        <span className="font-mono text-xs tabular-nums">
          {avgTrade ? `$${formatCompactUnits(avgTrade)}` : '—'}
        </span>
      </Td>
      <Td>
        <SourceSparkline buckets={m.volume_history_24h} />
      </Td>
      <Td align="right">
        <span className="text-ink-muted font-mono text-xs tabular-nums">
          {formatRelative(m.last_trade_at)}
        </span>
      </Td>
    </tr>
  );
}

function sideTicker(id: string): string {
  const up = id.toUpperCase();
  if (up === 'NATIVE' || /^\d+$/.test(up)) return 'XLM';
  return up.replace(/^(CRYPTO|FIAT):/, '');
}

function shortAsset(canonical: string): string {
  if (canonical === 'native') return 'XLM';
  if (canonical.startsWith('crypto:')) return canonical.replace('crypto:', '');
  if (/^C[A-Z2-7]{55}$/.test(canonical))
    return `${canonical.slice(0, 4)}…${canonical.slice(-4)} (SAC)`;
  if (canonical.startsWith('fiat:')) return canonical.replace('fiat:', '');
  if (/^\d+$/.test(canonical)) return 'XLM';
  const dashIx = canonical.indexOf('-');
  if (dashIx === -1) return canonical;
  const code = canonical.slice(0, dashIx);
  const issuer = canonical.slice(dashIx + 1);
  return `${code} (${issuer.slice(0, 6)}…${issuer.slice(-4)})`;
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
      className={`px-4 py-2 ${align === 'right' ? 'text-right' : 'text-left'}`}
      scope="col"
    >
      {children}
    </th>
  );
}

function Td({
  children,
  align,
}: {
  children: React.ReactNode;
  align?: 'left' | 'right';
}) {
  return (
    <td
      className={`px-4 py-3 ${align === 'right' ? 'text-right' : 'text-left'}`}
    >
      {children}
    </td>
  );
}
