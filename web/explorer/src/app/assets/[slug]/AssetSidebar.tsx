'use client';

import Link from 'next/link';

import { asExample } from '@/api/client';
import { useAsset } from '@/api/hooks';
import { Panel } from '@/components/reveal';
import { RequestReveal } from '@/components/reveal/RequestReveal';
import { CURRENT_NETWORK } from '@/lib/networks';
import { formatCompact, positiveDecimal } from '@/lib/format';
import { isSafeHomeDomain } from '@/lib/safe-domain';
import { PANEL_LABEL_CLASS } from '@/components/ui/Page';
import { AssetSwap } from './AssetSwap';
import { ChangeSummaryStrip } from './ChangeSummaryStrip';
import { LiveAssetPrice, type PriceProvenance } from './LiveAssetPrice';
import { PerformanceRange } from './PerformanceRange';
import { bakedAtDeploy } from './priceProvenance';
import { SidebarAssetIcon } from './SidebarAssetIcon';
import {
  VOLUME_FLOOR_HINT,
  supplyHint,
  supplyIsDerived,
  supplyIsFloor,
  supplyMark,
} from '@/lib/supplyFloor';

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
  volume_lower_bound?: boolean;
  market_cap_usd?: string | null;
  circulating_supply?: string | null;
  supply_basis?: string | null;
  value_lower_bound?: boolean;
  market_cap_lower_bound?: boolean;
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
  volume_lower_bound?: boolean;
  market_cap_lower_bound?: boolean;
  supply_basis?: string | null;
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
  detail: bakedDetail,
  priceUSD,
  priceProvenance,
  priceStale,
  homeDomain,
}: {
  coin: SidebarCoin;
  detail: SidebarDetail | null;
  /** Wire decimal string; the converter and range bar compute in floats. */
  priceUSD: string | null;
  priceProvenance?: PriceProvenance;
  priceStale?: boolean;
  homeDomain?: string | null;
}) {
  const seed = positiveDecimal(priceUSD);
  // The live row replaces the baked figures whole, so a USD figure it
  // serves null (a withheld price, a refused cap) stays blank.
  const live = useAsset(coin.asset_id).data?.data ?? null;
  const detail: SidebarDetail | null = live ?? bakedDetail;
  const activity = live ?? coin;
  const bakedHint = live ? undefined : bakedAtDeploy();
  // market_cap / volume / fdv arrive already server-pre-scaled (USD) —
  // num() only, never divide. The supply fields (circulating / total /
  // max) are RAW smallest-unit integers, so scale them down 10^decimals.
  const marketCap = num((detail ?? coin).market_cap_usd);
  const vol = num((detail ?? coin).volume_24h_usd);
  const fdv = num(detail?.fdv_usd);
  const decimals = detail?.decimals ?? coin.decimals;
  const circulating = supplyNum(
    detail?.circulating_supply ?? coin.circulating_supply,
    decimals,
  );
  const total = supplyNum(detail?.total_supply, decimals);
  const max = supplyNum(detail?.max_supply, decimals);
  const volMktCap =
    vol != null && marketCap != null && marketCap > 0 ? vol / marketCap : null;
  // Floors come from the same payload as the figure they qualify.
  const volFloor = (detail ?? coin).volume_lower_bound === true;
  const capFloor = supplyIsFloor(detail ?? coin);
  const circSrc = detail?.circulating_supply != null ? detail : coin;
  const circFloor = supplyIsFloor(circSrc);
  const circDerived = !circFloor && supplyIsDerived(circSrc);
  // A floor over an exact cap is "at least", an exact volume over a floor
  // cap is "at most"; with both floors the ratio is bounded on neither side.
  const derived = !capFloor && supplyIsDerived(detail ?? coin);
  const ratioMark =
    volFloor && capFloor
      ? null
      : derived
        ? null
        : volFloor
          ? '≥ '
          : capFloor
            ? '≤ '
            : '';
  const capTitle = supplyHint(capFloor, derived);
  const circTitle = supplyHint(circFloor, circDerived);
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
        hint={bakedHint}
        source={source}
        headingLevel={2}
        bodyClassName="divide-line -mx-4 -mb-4 divide-y"
      >
        <StatRow
          label="Market cap"
          value={floorMark(usd(marketCap), capFloor, derived)}
          title={capTitle}
        />
        <StatRow
          label="Volume (24h)"
          value={floorMark(usd(vol), volFloor)}
          title={volFloor ? VOLUME_FLOOR_HINT : undefined}
        />
        <StatRow
          label="Vol / Mkt Cap"
          value={
            volMktCap != null && ratioMark != null
              ? `${ratioMark}${(volMktCap * 100).toFixed(2)}%`
              : '—'
          }
        />
        <StatRow label="FDV" value={usd(fdv)} />
        <StatRow
          label="Total supply"
          value={floorMark(supply(total, code), capFloor, derived)}
          title={capTitle}
        />
        <StatRow
          label="Max supply"
          value={max != null ? supply(max, code) : '—'}
        />
        <div className="px-4 py-2.5">
          <div className="flex items-center justify-between gap-3">
            <span className={PANEL_LABEL_CLASS}>Circulating supply</span>
            <span
              className="text-ink min-w-0 text-right font-mono text-sm tabular-nums"
              title={circTitle}
            >
              {floorMark(supply(circulating, code), circFloor, circDerived)}
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
        hint={bakedHint}
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
            activity.trade_count_24h != null
              ? formatCompact(activity.trade_count_24h)
              : '—'
          }
        />
        <StatRow
          label="Observations"
          value={
            activity.observation_count != null
              ? formatCompact(activity.observation_count)
              : '—'
          }
        />
        <StatRow
          label="Markets (24h)"
          value={
            activity.markets_count != null
              ? activity.markets_count.toLocaleString('en-US')
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
            priceUSD={seed}
            priceProvenance={priceProvenance ?? null}
            priceStale={priceStale}
            decimals={decimals}
          />
          <PerformanceRange
            assetId={coin.asset_id}
            priceUSD={seed}
            priceProvenance={priceProvenance ?? null}
            priceStale={priceStale}
            points={coin.price_history_24h ?? []}
            source={source}
          />
        </>
      )}
    </div>
  );
}

/** floorMark prefixes "≥" to a floor figure and "≤" to a derived supply reading. */
function floorMark(text: string, floor: boolean, derived = false): string {
  return text !== '—' ? `${supplyMark(floor, derived)}${text}` : text;
}

function StatRow({
  label,
  value,
  title,
}: {
  label: string;
  value: React.ReactNode;
  title?: string;
}) {
  return (
    <div className="flex items-center justify-between gap-3 px-4 py-2.5">
      <span className={PANEL_LABEL_CLASS}>{label}</span>
      <span
        className="text-ink min-w-0 text-right font-mono text-sm tabular-nums"
        title={title}
      >
        {value}
      </span>
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
  decimals: number | null | undefined,
): number | null {
  const n = num(raw);
  return n != null && decimals != null ? n / 10 ** decimals : null;
}

function usd(n: number | null): string {
  return n != null ? `$${formatCompact(n)}` : '—';
}

function supply(n: number | null, code: string): string {
  return n != null ? `${formatCompact(n)} ${code}` : '—';
}
