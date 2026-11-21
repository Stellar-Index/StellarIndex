'use client';

import Link from 'next/link';

import { useMemo } from 'react';

import { Panel } from '@/components/reveal';
import {
  Callout,
  Skeleton,
  Table,
  THead,
  TBody,
  Th,
  Td,
  LinkRow,
} from '@/components/ui';
import { asExample } from '@/api/client';
import { SourceSparkline } from '@/components/SourceSparkline';
import { useMarkets, useVerifiedIndex, type Market } from '@/api/hooks';
import { isNativeXlmSac } from '@/lib/asset-label';
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

  // A next cursor or the truncated flag means the server holds more rows than
  // this page: the count is a lower bound, never a total.
  const truncated =
    Boolean(markets.data?.nextCursor) ||
    Boolean(markets.data?.flags?.truncated);

  if (markets.isError) {
    return (
      <Panel
        headingLevel={2}
        title="Markets"
        source={asExample('/v1/markets', { limit: 100 })}
      >
        <Callout tone="bad">Failed to load markets.</Callout>
      </Panel>
    );
  }
  if (markets.isLoading) {
    return (
      <Panel
        headingLevel={2}
        title="Markets"
        source={asExample('/v1/markets', { limit: 100 })}
      >
        <div className="space-y-3" aria-busy="true">
          <span className="sr-only">Loading markets</span>
          <Skeleton className="h-4 w-3/5" aria-hidden />
          <Skeleton className="h-4 w-full" aria-hidden />
          <Skeleton className="h-4 w-4/5" aria-hidden />
        </div>
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
        <Table>
          <THead>
            <tr>
              <Th>Pair</Th>
              <Th align="right">24h volume</Th>
              <Th align="right">24h trades</Th>
              <Th align="right">Avg trade</Th>
              <Th>24h chart</Th>
              <Th align="right">Last trade</Th>
            </tr>
          </THead>
          <TBody>
            {matched.map((m) => (
              <Row
                key={`${m.base}|${m.quote}`}
                m={m}
                assetID={assetID}
                barMax={barMax}
              />
            ))}
          </TBody>
        </Table>
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
  const { data: verified } = useVerifiedIndex();
  // The server expands a catalogue slug ("usdc") into its asset_ids. A
  // classic leg is the asset only as its verified issuance: anyone can
  // issue a "USDC-G…" or "XLM-G…", so a code prefix is no identity.
  const want = sideTicker(assetID);
  const issuance = isCatalogueSlug(assetID)
    ? verified?.issuanceBySlug?.get(assetID.toLowerCase())
    : undefined;
  const matches = (side: string) => {
    if (side === assetID || side === issuance) return true;
    if (want === 'XLM' && isXlmIdentity(assetID)) return isXlmIdentity(side);
    return /^(crypto|fiat):/i.test(side) && sideTicker(side) === want;
  };
  const isBase = matches(m.base ?? '');
  const counterparty = isBase ? m.quote : m.base;
  const pairSlug = `${m.base}~${m.quote}`;
  // A floor divided by the full trade count is not an average.
  const floor = m.volume_lower_bound === true;
  const avgTrade = floor
    ? null
    : divideDecimalString(m.volume_24h_usd, m.trade_count_24h);
  return (
    <LinkRow href={`/markets/${encodeURIComponent(pairSlug)}/`}>
      <Td className="whitespace-nowrap">
        {/* Rows link their pair page. */}
        <Link
          href={`/markets/${encodeURIComponent(pairSlug)}/`}
          className="hover:text-brand-600"
        >
          <span className="font-medium">vs </span>
          <span className="font-mono text-xs">
            {shortAsset(counterparty ?? '')}
          </span>
        </Link>
        <span className="text-ink-faint ml-2 text-xs">
          {isBase ? 'base' : 'quote'}
        </span>
      </Td>
      <Td align="right">
        <span
          className="font-mono text-xs"
          title={
            floor
              ? 'Lower bound: trades without a trade-time USD value are excluded.'
              : undefined
          }
        >
          {m.volume_24h_usd
            ? `${floor ? '≥ ' : ''}$${formatCompactUnits(m.volume_24h_usd)}`
            : '—'}
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
        <span className="font-mono">{formatCompact(m.trade_count_24h)}</span>
      </Td>
      <Td align="right">
        <span className="font-mono text-xs">
          {avgTrade ? `$${formatCompactUnits(avgTrade)}` : '—'}
        </span>
      </Td>
      <Td>
        <SourceSparkline buckets={m.volume_history_24h} />
      </Td>
      <Td align="right">
        <span className="text-ink-muted font-mono text-xs">
          {formatRelative(m.last_trade_at)}
        </span>
      </Td>
    </LinkRow>
  );
}

// A bare catalogue slug such as "usdc", not an asset id of any form.
function isCatalogueSlug(id: string): boolean {
  return /^[a-z0-9]+$/i.test(id) && !/^\d+$/.test(id) && id !== 'native';
}

// XLM's disjoint identities: native, its CEX ticker, its SAC, the slug.
function isXlmIdentity(id: string): boolean {
  const up = id.toUpperCase();
  return (
    up === 'NATIVE' ||
    up === 'XLM' ||
    up === 'CRYPTO:XLM' ||
    /^\d+$/.test(up) ||
    isNativeXlmSac(id)
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
