import Link from 'next/link';

import { formatCompact, formatPriceSmall } from '@/lib/format';
import { isSafeHomeDomain } from '@/lib/safe-domain';
import { AssetConverter } from './AssetConverter';
import { SidebarAssetIcon } from './SidebarAssetIcon';

// Loosely-typed mirror of the page's fetched shapes — only the fields
// the sidebar renders. Keeps this component decoupled from page.tsx's
// internal interfaces.
export interface SidebarCoin {
  code: string;
  // SEP-1 icon URL (served by the API since v0.7.2; https-only,
  // sanitized server-side). Letter glyph stays the fallback.
  image?: string | null;
  slug: string;
  asset_id: string;
  // On-chain smallest-unit scale — 7 for classic / native, 0 for
  // catalogue / fiat rows. Supply strings are RAW smallest-unit
  // integers and must be scaled down 10^decimals before display.
  decimals?: number | null;
  issuer?: string | null;
  price_usd?: string | null;
  volume_24h_usd?: string | null;
  market_cap_usd?: string | null;
  circulating_supply?: string | null;
  change_24h_pct?: string | null;
  trade_count_24h?: number | null;
  observation_count: number;
  markets_count?: number | null;
  price_history_24h?: { t: string; p?: string | null }[];
  ath?: { usd: string; at: string } | null;
}

export interface SidebarDetail {
  decimals?: number | null;
  total_supply?: string | null;
  circulating_supply?: string | null;
  max_supply?: string | null;
  market_cap_usd?: string | null;
  fdv_usd?: string | null;
  volume_24h_usd?: string | null;
  home_domain?: string | null;
}

/**
 * AssetSidebar — the dense left-column stats rail (stellarchain-grade):
 * identity + live price, market/supply stats with a circulating-supply
 * progress bar, the issuer/links block, a USD converter, and a 24h
 * price-performance range. Mirrors the reference USDC layout (Image #5).
 */
export function AssetSidebar({
  coin,
  detail,
  priceUSD,
  name,
  homeDomain,
}: {
  coin: SidebarCoin;
  detail: SidebarDetail | null;
  priceUSD: number | null;
  name?: string | null;
  homeDomain?: string | null;
}) {
  // market_cap / volume / fdv arrive already server-pre-scaled (USD) —
  // num() only, never divide. The supply fields (circulating / total /
  // max) are RAW smallest-unit integers, so scale them down 10^decimals.
  const marketCap = num(detail?.market_cap_usd ?? coin.market_cap_usd);
  const vol = num(detail?.volume_24h_usd ?? coin.volume_24h_usd);
  const fdv = num(detail?.fdv_usd);
  const decimals = detail?.decimals ?? coin.decimals ?? 7;
  const circulating = supplyNum(
    detail?.circulating_supply ?? coin.circulating_supply,
    decimals,
  );
  const total = supplyNum(detail?.total_supply, decimals);
  const max = supplyNum(detail?.max_supply, decimals);
  const volMktCap = vol != null && marketCap != null && marketCap > 0 ? vol / marketCap : null;
  const change = num(coin.change_24h_pct);
  // Native (and a few sparse rows) can arrive without a `code` — fall
  // back to the slug so the avatar glyph + labels never crash on slice.
  const code = coin.code || coin.slug || '—';

  // Circulating-supply progress: against max if known, else total.
  const denom = max ?? total ?? null;
  // AM-22: when neither max nor total is known, the bar previously
  // asserted 100% — fabricated certainty. Unknown renders as no bar.
  const circPct =
    circulating != null && denom != null && denom > 0
      ? Math.min(100, (circulating / denom) * 100)
      : null;

  return (
    <div className="space-y-4">
      {/* Identity + live price */}
      <div className="rounded-card border border-line bg-surface p-4">
        <div className="flex items-center gap-2.5">
          <SidebarAssetIcon image={coin.image} code={code} />
          <div className="min-w-0">
            <div className="flex items-baseline gap-1.5">
              <span className="text-lg font-semibold text-ink">{code}</span>
              {name && name !== code && (
                <span className="truncate text-xs text-ink-muted">{name}</span>
              )}
            </div>
          </div>
        </div>
        <div className="mt-3 flex flex-wrap items-baseline gap-2">
          <span className="font-mono text-3xl tabular-nums text-ink">
            {priceUSD != null ? `$${formatPriceSmall(priceUSD)}` : '—'}
          </span>
          {change != null && (
            <span
              className={`font-mono text-sm tabular-nums ${
                change > 0 ? 'text-up' : change < 0 ? 'text-down' : 'text-ink-muted'
              }`}
            >
              {change > 0 ? '▲' : change < 0 ? '▼' : ''} {change > 0 ? '+' : ''}
              {change.toFixed(2)}% <span className="text-ink-faint">(24h)</span>
            </span>
          )}
        </div>
      </div>

      {/* Market + supply stats */}
      <div className="rounded-card border border-line bg-surface">
        <StatRow label="Market cap" value={usd(marketCap)} />
        <StatRow label="Volume (24h)" value={usd(vol)} />
        <StatRow label="Vol / Mkt Cap" value={volMktCap != null ? `${(volMktCap * 100).toFixed(2)}%` : '—'} />
        <StatRow label="FDV" value={usd(fdv)} />
        <StatRow label="Total supply" value={supply(total, code)} />
        <StatRow label="Max supply" value={max != null ? supply(max, code) : '—'} />
        <div className="px-4 py-3">
          <div className="flex items-baseline justify-between">
            <span className="text-[11px] uppercase tracking-wider text-ink-muted">
              Circulating supply
            </span>
            <span className="font-mono text-xs tabular-nums text-ink-body">
              {circPct != null ? `${circPct.toFixed(circPct >= 99.95 ? 0 : 1)}%` : '—'}
            </span>
          </div>
          {circPct != null && (
            <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-surface-muted">
              <div className="h-full rounded-full bg-brand-500" style={{ width: `${circPct}%` }} />
            </div>
          )}
          <div className="mt-1.5 font-mono text-sm tabular-nums text-ink">
            {supply(circulating, code)}
          </div>
        </div>
      </div>

      {/* Links / issuer / activity */}
      <div className="rounded-card border border-line bg-surface">
        {/* CS-102: home_domain is attacker-controlled on-chain data; only
            render it as a clickable link when it passes isSafeHomeDomain
            (the guard the issuer pages already use). Otherwise show plain
            text so a smuggled userinfo/path can't produce a phishing link. */}
        {homeDomain && (
          <StatRow
            label="Website"
            value={
              isSafeHomeDomain(homeDomain) ? (
                <a
                  href={`https://${homeDomain}`}
                  target="_blank"
                  rel="noreferrer noopener nofollow"
                  className="font-mono text-xs text-brand-600 hover:underline"
                >
                  {homeDomain}
                </a>
              ) : (
                <span className="font-mono text-xs text-ink-muted">{homeDomain}</span>
              )
            }
          />
        )}
        {coin.issuer && (
          <StatRow
            label="Issuer"
            value={
              <Link
                href={`/issuers/${coin.issuer}`}
                className="font-mono text-xs text-brand-600 hover:underline"
                title={coin.issuer}
              >
                {coin.issuer.slice(0, 4)}…{coin.issuer.slice(-4)}
              </Link>
            }
          />
        )}
        <StatRow
          label="Trades (24h)"
          value={coin.trade_count_24h != null ? formatCompact(coin.trade_count_24h) : '—'}
        />
        <StatRow label="Observations" value={formatCompact(coin.observation_count)} />
        <StatRow
          label="Markets (24h)"
          value={coin.markets_count != null ? coin.markets_count.toLocaleString() : '—'}
        />
      </div>

      {/* Converter */}
      <AssetConverter symbol={code} priceUSD={priceUSD} />

      {/* 24h price performance range */}
      <PerformanceRange points={coin.price_history_24h ?? []} current={priceUSD} />
    </div>
  );
}

function StatRow({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between border-b border-line px-4 py-2.5 last:border-0">
      <span className="text-[11px] uppercase tracking-wider text-ink-muted">{label}</span>
      <span className="font-mono text-sm tabular-nums text-ink">{value}</span>
    </div>
  );
}

function PerformanceRange({
  points,
  current,
}: {
  points: { t: string; p?: string | null }[];
  current: number | null;
}) {
  const vals = points
    .map((pt) => (pt.p != null ? Number(pt.p) : null))
    .filter((v): v is number => v != null && Number.isFinite(v));
  if (vals.length < 2) return null;
  const low = Math.min(...vals);
  const high = Math.max(...vals);
  const cur = current ?? vals[vals.length - 1];
  const pct = high > low ? Math.max(0, Math.min(100, ((cur - low) / (high - low)) * 100)) : 50;
  return (
    <div className="rounded-card border border-line bg-surface p-4">
      <div className="flex items-baseline justify-between">
        <span className="text-[11px] uppercase tracking-wider text-ink-muted">
          Price performance
        </span>
        <span className="text-[10px] uppercase tracking-wider text-ink-faint">24h</span>
      </div>
      <div className="mt-2 flex items-center justify-between text-[11px] text-ink-muted">
        <span>Low</span>
        <span>High</span>
      </div>
      <div className="relative mt-1 h-1.5 rounded-full bg-linear-to-r from-down via-warn-500 to-up">
        <div
          className="absolute top-1/2 h-3 w-1 -translate-y-1/2 rounded-full bg-ink shadow-sm"
          style={{ left: `calc(${pct}% - 2px)` }}
          aria-hidden
        />
      </div>
      <div className="mt-1.5 flex items-baseline justify-between font-mono text-xs tabular-nums text-ink-body">
        <span>${formatPriceSmall(low)}</span>
        <span>${formatPriceSmall(high)}</span>
      </div>
    </div>
  );
}

function num(raw: string | null | undefined): number | null {
  if (raw == null || raw === '') return null;
  const n = Number(raw);
  return Number.isFinite(n) ? n : null;
}

// supplyNum parses a RAW smallest-unit supply string and scales it down
// 10^decimals to whole asset units (mirrors SupplyTabPanel.parseSmallest
// / HoldersTabPanel). Display-only — never re-used for arithmetic.
function supplyNum(
  raw: string | null | undefined,
  decimals: number,
): number | null {
  const n = num(raw);
  return n != null ? n / 10 ** decimals : null;
}

function usd(n: number | null): string {
  return n != null ? `$${formatCompact(n)}` : '—';
}

function supply(n: number | null, code: string): string {
  return n != null ? `${formatCompact(n)} ${code}` : '—';
}
