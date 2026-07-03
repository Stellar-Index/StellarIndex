'use client';

import Link from 'next/link';

import { useNativeUsdPrice, useNetworkStats, useSources } from '@/api/hooks';
import { Stat } from '@/components/ui';
import { formatCompact } from '@/lib/format';

/**
 * HomeNetworkStrip — 5-card network-level stats hero on the home
 * page. Sits above the existing NetworkLivePanel /
 * SystemHealthLivePanel grid, giving the user an immediate
 * scale-of-the-network read at the top of the page:
 *
 *   - Stellar ON-CHAIN 24h USD volume (SDEX + Soroban DEXes),
 *     summed from the DEX-subclass /v1/sources — NOT the all-source
 *     /v1/network/stats total, which the CEX feeds dominate
 *   - Active markets in the trailing 24h
 *   - Asset directory size (classic assets indexed)
 *   - Sources online (Class=exchange contributors to VWAP)
 *   - XLM price (with 24h change pill)
 *
 * Markets / assets / sources / ledger come from /v1/network/stats
 * (server-aggregated across the full corpus). The 24h volume is
 * computed from /v1/sources (DEX-subclass sum) so it's Stellar-only.
 *
 * XLM price comes from /v1/price?asset=native (the canonical VWAP).
 * It must NOT come from /v1/assets / useCoins — native XLM has no
 * classic_assets row, so it isn't in that listing at all; pulling
 * "the first coin" resolved to USDC (~$1.00) mislabelled as XLM.
 */
export function HomeNetworkStrip() {
  const stats = useNetworkStats();
  const sources = useSources(undefined, true);
  const native = useNativeUsdPrice();

  // Stellar ON-CHAIN 24h volume = sum of DEX-subclass sources (SDEX +
  // the Soroban DEXes). /v1/network/stats.volume_24h_usd is the
  // ALL-source total — dominated by the CEX feeds (Binance et al.),
  // which don't trade on Stellar — so it isn't "Stellar volume".
  const stellarVolume = (sources.data ?? [])
    .filter((s) => s.subclass === 'dex')
    .reduce((sum, s) => sum + (s.volume_24h_usd ? Number(s.volume_24h_usd) : 0), 0);
  const volume = stellarVolume > 0 ? stellarVolume : null;
  const activeMarkets = stats.data?.markets_count_24h ?? null;
  const assetsIndexed = stats.data?.assets_indexed ?? null;
  const exchangeSources = stats.data?.exchange_sources ?? null;
  const tipLedger = stats.data?.latest_ledger ?? null;

  // XLM price from the canonical native VWAP — NOT the coins list
  // (native has no classic_assets row, so it isn't in /v1/assets).
  const xlmPrice = native.price;
  const xlmChange = native.change24hPct;

  return (
    <section className="grid grid-cols-2 gap-3 md:grid-cols-5">
      <Cell
        label="24h volume"
        value={
          volume != null && Number.isFinite(volume) && volume > 0
            ? `$${formatCompact(volume)}`
            : '—'
        }
        sub="Stellar on-chain (SDEX + DEXes)"
        href="/markets"
      />
      <Cell
        label="Active markets"
        value={
          activeMarkets != null ? activeMarkets.toLocaleString() : '—'
        }
        sub="trading in last 24h"
        href="/markets"
      />
      <Cell
        label="Assets indexed"
        value={
          assetsIndexed != null ? formatCompact(assetsIndexed) : '—'
        }
        sub="classic + native"
        href="/assets"
      />
      <Cell
        label="Sources online"
        value={
          exchangeSources != null ? `${exchangeSources}` : '—'
        }
        sub="exchange feeds live"
        href="/sources"
      />
      {xlmPrice != null ? (
        <Cell
          label="XLM"
          value={`$${xlmPrice.toFixed(xlmPrice >= 1 ? 4 : 6)}`}
          sub={
            xlmChange != null && Number.isFinite(xlmChange)
              ? `${xlmChange > 0 ? '+' : ''}${xlmChange.toFixed(2)}% 24h`
              : 'no 24h baseline'
          }
          tone={
            xlmChange != null && Number.isFinite(xlmChange)
              ? xlmChange > 0
                ? 'up'
                : xlmChange < 0
                  ? 'down'
                  : undefined
              : undefined
          }
          href="/assets/XLM"
        />
      ) : (
        <Cell
          label="Ledger tip"
          value={tipLedger != null ? `#${tipLedger.toLocaleString()}` : '—'}
          sub="ingest cursor"
          mono
          href="/diagnostics"
        />
      )}
    </section>
  );
}

function Cell({
  label,
  value,
  sub,
  tone,
  mono,
  href,
}: {
  label: string;
  value: string;
  sub?: string;
  tone?: 'up' | 'down';
  mono?: boolean;
  href?: string;
}) {
  const subNode = sub ? (
    <span
      className={
        tone === 'up' ? 'text-up' : tone === 'down' ? 'text-down' : 'text-ink-muted'
      }
    >
      {sub}
    </span>
  ) : undefined;
  const inner = (
    <Stat
      label={label}
      value={
        <span className={`truncate ${mono ? 'font-mono' : ''}`} title={value}>
          {value}
        </span>
      }
      sub={subNode}
    />
  );
  const baseClass = 'block rounded-card border border-line bg-surface p-4';
  if (href) {
    return (
      <Link
        href={href}
        className={`${baseClass} shadow-card transition-all hover:border-line-strong hover:shadow-elevated`}
      >
        {inner}
      </Link>
    );
  }
  return <div className={`${baseClass} shadow-card`}>{inner}</div>;
}
