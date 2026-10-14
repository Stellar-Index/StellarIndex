import Link from 'next/link';

import { asExample } from '@/api/client';
import { Panel } from '@/components/reveal';
import { RequestReveal } from '@/components/reveal/RequestReveal';
import { CURRENT_NETWORK } from '@/lib/networks';
import { formatCompact, formatPriceSmall, positiveDecimal } from '@/lib/format';
import { isSafeHomeDomain } from '@/lib/safe-domain';
import { PANEL_LABEL_CLASS } from '@/components/ui/Page';
import { AssetSwap } from './AssetSwap';
import { ChangeSummaryStrip } from './ChangeSummaryStrip';
import { LiveAssetPrice, type PriceProvenance } from './LiveAssetPrice';
import { SidebarAssetIcon } from './SidebarAssetIcon';

// Loosely-typed mirror of the page's fetched shapes — only the fields
// the sidebar renders. Keeps this component decoupled from page.tsx's
// internal interfaces.
export interface SidebarCoin {
  // Optional: a Soroban contract asset has no `code` (the API omits the
  // field). Must stay optional: the consumers
  // below dereference it only after a presence check.
  code?: string;
  // SEP-1 icon URL (https-only,
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
 * AssetIdentityCard — icon, code, name, the live headline price and the
 * change strip. The page's first grid cell, so phones read it before the
 * issuer panel and tabs while the stats rail follows them.
 */
export function AssetIdentityCard({
  coin,
  priceUSD,
  priceProvenance,
  priceStale,
  name,
}: {
  coin: SidebarCoin;
  /** Wire decimal string. */
  priceUSD: string | null;
  /**
   * WHAT the headline number is — rendered as a caption so the page
   * never shows a bare dollar figure of unstated provenance
   * (a build-time listing snapshot up to 7 days old must not be
   * presented identically to a live VWAP).
   *  - 'vwap1m'       — /v1/price closed 1-minute VWAP (the real thing)
   *  - 'triangulated' — composed client-side from asset/XLM × XLM/USD
   *  - 'listing'      — build-time listing cache; can lag hours–days
   *  - 'global_market' — the vetted token's global ticker price across
   *                     venues (price_basis=global_market); not a
   *                     Stellar market observation
   *  - 'declared_peg' — server-filled operator-declared 1:1 fiat peg ×
   *                     current FX rate (price_basis=declared_peg); not
   *                     a market observation
   *  - 'transitive'   — two-hop DEX route (price_basis=transitive), both
   *                     legs substance-gated
   */
  priceProvenance?: PriceProvenance;
  priceStale?: boolean;
  name?: string | null;
}) {
  // Native (and a few sparse rows) can arrive without a `code` — fall
  // back to the slug so the avatar glyph + labels never crash on slice.
  const code = coin.code || coin.slug || '—';
  return (
    <div className="rounded-card border-line bg-surface border p-4">
      <div className="flex items-start justify-between gap-2">
        <div className="flex min-w-0 items-center gap-2.5">
          <SidebarAssetIcon image={coin.image} code={code} />
          <div className="flex min-w-0 items-baseline gap-1.5">
            <span className="text-ink text-lg font-semibold">{code}</span>
            {name && name !== code && (
              <span className="text-ink-muted truncate text-xs">{name}</span>
            )}
          </div>
        </div>
        <RequestReveal
          example={asExample('/v1/price', { asset: coin.asset_id })}
          position="inline"
        />
      </div>
      {/* The baked value paints first, then the browser re-fetches /v1/price
          on mount + every 60s: a static export must never present a
          build-frozen number as the current price. */}
      <LiveAssetPrice
        assetID={coin.asset_id}
        initialPrice={priceUSD}
        initialProvenance={priceProvenance ?? null}
        initialStale={priceStale}
        initialChangePct={num(coin.change_24h_pct)}
      />
      <ChangeSummaryStrip assetID={coin.asset_id} className="mt-3" />
    </div>
  );
}

/**
 * AssetSidebar — the stats rail: market and supply figures with the
 * circulating-supply bar, the issuer/links/activity rows, a USD converter
 * and the 24h price range. Every panel reveals the /v1/assets call behind it.
 */
export function AssetSidebar({
  coin,
  detail,
  priceUSD,
  homeDomain,
}: {
  coin: SidebarCoin;
  detail: SidebarDetail | null;
  /** Wire decimal string; the converter and range bar compute in floats. */
  priceUSD: string | null;
  homeDomain?: string | null;
}) {
  const seed = positiveDecimal(priceUSD);
  const priceNum = seed != null ? Number(seed) : null;
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
  const volMktCap =
    vol != null && marketCap != null && marketCap > 0 ? vol / marketCap : null;
  const code = coin.code || coin.slug || '—';
  const source = asExample(`/v1/assets/${coin.asset_id}`);

  // Circulating-supply progress: against max if known, else total. When
  // neither is known, asserting 100% would be fabricated certainty.
  const denom = max ?? total ?? null;
  const circPct =
    circulating != null && denom != null && denom > 0
      ? Math.min(100, (circulating / denom) * 100)
      : null;

  return (
    <div className="space-y-4">
      <Panel
        title="Market"
        source={source}
        headingLevel={2}
        bodyClassName="divide-line -mx-4 -mb-4 divide-y"
      >
        <StatRow label="Market cap" value={usd(marketCap)} />
        <StatRow label="Volume (24h)" value={usd(vol)} />
        <StatRow
          label="Vol / Mkt Cap"
          value={volMktCap != null ? `${(volMktCap * 100).toFixed(2)}%` : '—'}
        />
        <StatRow label="FDV" value={usd(fdv)} />
        <StatRow label="Total supply" value={supply(total, code)} />
        <StatRow
          label="Max supply"
          value={max != null ? supply(max, code) : '—'}
        />
        <div className="px-4 py-2.5">
          <div className="flex items-center justify-between gap-3">
            <span className={PANEL_LABEL_CLASS}>Circulating supply</span>
            <span className="text-ink min-w-0 text-right font-mono text-sm tabular-nums">
              {supply(circulating, code)}
            </span>
          </div>
          {circPct != null && (
            <div className="mt-2 flex items-center gap-3">
              <div className="bg-surface-muted h-1.5 flex-1 overflow-hidden rounded-full">
                <div
                  className="bg-brand-500 h-full rounded-full"
                  style={{ width: `${circPct}%` }}
                />
              </div>
              <span className="text-ink-muted font-mono text-xs tabular-nums">
                {`${circPct.toFixed(circPct >= 99.95 ? 0 : 1)}%`}
              </span>
            </div>
          )}
        </div>
      </Panel>

      <Panel
        title={coin.issuer || homeDomain ? 'Issuer & activity' : 'Activity'}
        source={source}
        headingLevel={2}
        bodyClassName="divide-line -mx-4 -mb-4 divide-y"
      >
        {/* home_domain is attacker-controlled on-chain data; only
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
                  className="text-brand-600 font-mono text-sm hover:underline"
                >
                  {homeDomain}
                </a>
              ) : (
                <span className="text-ink-muted font-mono text-sm">
                  {homeDomain}
                </span>
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
                className="text-brand-600 font-mono text-sm hover:underline"
                title={coin.issuer}
              >
                {coin.issuer.slice(0, 4)}…{coin.issuer.slice(-4)}
              </Link>
            }
          />
        )}
        <StatRow
          label="Trades (24h)"
          value={
            coin.trade_count_24h != null
              ? formatCompact(coin.trade_count_24h)
              : '—'
          }
        />
        <StatRow
          label="Observations"
          value={formatCompact(coin.observation_count)}
        />
        <StatRow
          label="Markets (24h)"
          value={
            coin.markets_count != null
              ? coin.markets_count.toLocaleString('en-US')
              : '—'
          }
        />
      </Panel>

      {/* Converter + 24h USD performance range — pricing-only; hidden on the
          lean test nets (no aggregator), where they'd be empty/inert. */}
      {CURRENT_NETWORK.pricing && (
        <>
          <AssetSwap
            symbol={code}
            assetId={coin.asset_id}
            image={coin.image}
            priceUSD={priceNum}
          />
          <PerformanceRange
            points={coin.price_history_24h ?? []}
            current={priceNum}
            source={source}
          />
        </>
      )}
    </div>
  );
}

function StatRow({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-3 px-4 py-2.5">
      <span className={PANEL_LABEL_CLASS}>{label}</span>
      <span className="text-ink min-w-0 text-right font-mono text-sm tabular-nums">
        {value}
      </span>
    </div>
  );
}

function PerformanceRange({
  points,
  current,
  source,
}: {
  points: { t: string; p?: string | null }[];
  current: number | null;
  source: ReturnType<typeof asExample>;
}) {
  const vals = points
    .map((pt) => (pt.p != null ? Number(pt.p) : null))
    .filter((v): v is number => v != null && Number.isFinite(v));
  if (vals.length < 2) return null;
  const low = Math.min(...vals);
  const high = Math.max(...vals);
  const cur = current ?? vals[vals.length - 1];
  const pct =
    high > low
      ? Math.max(0, Math.min(100, ((cur - low) / (high - low)) * 100))
      : 50;
  return (
    <Panel
      headingLevel={2}
      title="Price range"
      hint="24h · USD"
      source={source}
    >
      <div className="text-ink-muted flex items-center justify-between text-[11px]">
        <span>Low</span>
        <span>High</span>
      </div>
      <div className="bg-surface-muted relative mt-1 h-1.5 rounded-full">
        <div
          className="bg-ink absolute top-1/2 h-3 w-0.5 -translate-y-1/2 rounded-full"
          style={{ left: `calc(${pct}% - 1px)` }}
          aria-hidden
        />
      </div>
      <div className="text-ink-body mt-1.5 flex items-baseline justify-between font-mono text-xs tabular-nums">
        <span>${formatPriceSmall(low)}</span>
        <span>${formatPriceSmall(high)}</span>
      </div>
    </Panel>
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
