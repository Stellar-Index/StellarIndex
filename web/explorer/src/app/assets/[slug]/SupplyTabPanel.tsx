'use client';

import dynamic from 'next/dynamic';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { useAsset, useAssetSupply, type AssetSupply } from '@/api/hooks';
import { FreshnessMarker } from '@/components/primitives';
import { formatBaseUnits, formatCompact, scaleBaseUnits } from '@/lib/format';
import { type Envelope } from '../../explorer-shared';
import { SupplyFlowsBar, buildSupplyFlowRows } from './SupplyFlowsBar';

// Lazy-load the chart (~155 KB lightweight-charts) — only the supply
// tab needs it, and only when there's market-cap history to draw.
const MarketCapLineChart = dynamic(
  () => import('@/components/charts/LineChart').then((m) => m.LineChart),
  { ssr: false, loading: () => <div className="h-[260px]" /> },
);

/**
 * SupplyTabPanel — backs the "Supply" tab on /assets/[slug].
 *
 * Renders the F2 supply-derivation fields per ADR-0011:
 * circulating, total, max supply (smallest-integer-unit decimal
 * strings, divided by 10^decimals at display time), market cap,
 * fully-diluted valuation, and the supply_basis tag identifying
 * which policy produced the numbers. SEP-1 issuance declarations
 * (fixed_number / max_number / is_unlimited) appear when the
 * issuer published them.
 */
export function SupplyTabPanel({ assetID }: { assetID: string }) {
  const asset = useAsset(assetID);
  // Live on-chain supply (ADR-0034 supply_flows). Independent of the F2
  // snapshot below; 404s for unmapped classic assets → section omitted.
  const onchain = useAssetSupply(assetID);

  if (asset.isError) {
    return (
      <Panel
        headingLevel={2}
        title="Supply"
        source={asExample('/v1/assets/{asset_id}', { asset_id: assetID })}
        bodyClassName="text-sm text-down-strong"
      >
        Failed to load supply data.
      </Panel>
    );
  }

  if (asset.isLoading) {
    return (
      <Panel
        headingLevel={2}
        title="Supply"
        source={asExample('/v1/assets/{asset_id}', { asset_id: assetID })}
        bodyClassName="text-sm text-ink-muted"
      >
        Loading…
      </Panel>
    );
  }

  const a = asset.data?.data;
  if (!a) {
    return (
      <Panel
        headingLevel={2}
        title="Supply"
        source={asExample('/v1/assets/{asset_id}', { asset_id: assetID })}
        bodyClassName="text-sm text-ink-muted"
      >
        No asset detail available.
      </Panel>
    );
  }

  const decimals = a.decimals ?? 7;
  const circulating = parseSmallest(a.circulating_supply, decimals);
  const total = parseSmallest(a.total_supply, decimals);
  const max = parseSmallest(a.max_supply, decimals);

  const noSupply =
    a.circulating_supply == null &&
    a.total_supply == null &&
    a.max_supply == null;

  return (
    <Panel
      headingLevel={2}
      title="Supply"
      source={asExample('/v1/assets/{asset_id}', { asset_id: assetID })}
      bodyClassName="space-y-4"
    >
      <FreshnessMarker flags={asset.data?.flags} className="block" />
      {onchain.data?.data && (
        <OnChainSupply env={onchain.data} assetDecimals={a.decimals} />
      )}
      {noSupply ? (
        <p className="text-ink-muted text-sm">
          {onchain.data
            ? 'No ADR-0011 circulating/max breakdown for this asset yet — the live on-chain total above is sourced directly from mint/burn flows.'
            : 'No supply snapshot available for this asset. The supply observer may not have backfilled it yet.'}
        </p>
      ) : (
        <>
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
            <Metric
              label="Circulating"
              value={circulating != null ? formatCompact(circulating) : '—'}
              sublabel={`In smallest unit: ${a.circulating_supply ?? '—'}`}
            />
            <Metric
              label="Total"
              value={total != null ? formatCompact(total) : '—'}
              sublabel={a.is_unlimited ? 'Issuer asserts unbounded' : ''}
            />
            <Metric
              label="Max"
              value={max != null ? formatCompact(max) : '—'}
              sublabel={a.is_unlimited === true ? 'Unlimited' : ''}
            />
          </div>

          {(a.market_cap_usd || a.fdv_usd) && (
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <Metric
                label="Market cap (USD)"
                value={a.market_cap_usd ? formatUSD(a.market_cap_usd) : '—'}
                sublabel="circulating × USD price"
              />
              <Metric
                label="Fully diluted (USD)"
                value={a.fdv_usd ? formatUSD(a.fdv_usd) : '—'}
                sublabel="max supply × USD price"
              />
            </div>
          )}

          <MarketCapChart assetID={assetID} />

          {a.supply_basis && (
            <p className="text-ink-muted text-xs">
              <span className="font-mono">supply_basis</span>: {a.supply_basis}
              {' — '}
              policy under ADR-0011 that produced these numbers.
            </p>
          )}

          {(a.fixed_number || a.max_number || a.is_unlimited != null) && (
            <div className="border-line bg-surface-muted rounded-lg border p-3 text-xs">
              <h3 className="text-ink-muted mb-1 font-semibold tracking-wider uppercase">
                SEP-1 issuance declarations
              </h3>
              <p className="text-ink-body">
                What the issuer pledged in their{' '}
                <span className="font-mono">stellar.toml</span>— distinct from
                the live-ledger numbers above.
              </p>
              <ul className="mt-2 space-y-1 font-mono">
                {a.fixed_number && <li>fixed_number = {a.fixed_number}</li>}
                {a.max_number && <li>max_number = {a.max_number}</li>}
                {a.is_unlimited != null && (
                  <li>is_unlimited = {a.is_unlimited ? 'true' : 'false'}</li>
                )}
              </ul>
            </div>
          )}
        </>
      )}
    </Panel>
  );
}

// MarketCapChart renders the historical market-cap timeline for an
// asset: daily USD price × daily circulating supply (the supply_1d
// CAGG), served by GET /v1/chart?price_type=market_cap. On-chain assets
// (native / classic / Soroban) chart; off-chain crypto:* reference
// assets (no on-chain supply) return an empty series → a concise note.
function MarketCapChart({ assetID }: { assetID: string }) {
  const q = useQuery<{ points: { t: string; p: string }[] }>({
    queryKey: ['/v1/chart', 'market_cap', assetID],
    retry: false,
    queryFn: async () => {
      const env = await apiGet<
        Envelope<{ points: { t: string; p: string }[] }>
      >('/v1/chart', {
        asset: assetID,
        quote: 'fiat:USD',
        price_type: 'market_cap',
        timeframe: '1y',
        granularity: '1d',
      });
      return env.data;
    },
    staleTime: 60_000,
  });

  const points = (q.data?.points ?? []).map((pt) => ({
    time: Math.floor(Date.parse(pt.t) / 1000),
    value: Number(pt.p),
  }));

  return (
    <div className="border-line bg-surface rounded-lg border p-4">
      <h3 className="text-ink-muted mb-2 text-xs font-semibold tracking-wider uppercase">
        Market-cap timeline
      </h3>
      {q.isLoading && <div className="h-[260px]" />}
      {/* A failed /v1/chart used to fall straight into the empty state
          and assert "no market-cap history for this asset". Absent is
          not empty. */}
      {!q.isLoading && q.isError && (
        <p className="text-ink-muted text-sm">
          Market-cap history unavailable right now — the series query
          didn&apos;t return. Retry shortly.
        </p>
      )}
      {!q.isLoading && !q.isError && points.length < 2 && (
        <p className="text-ink-muted text-sm">
          No market-cap history for this asset — it needs both an on-chain
          circulating supply and a USD price track over time.
        </p>
      )}
      {points.length >= 2 && (
        <MarketCapLineChart
          data={points}
          ariaLabel={`Daily USD market cap for ${assetID} over the last year`}
        />
      )}
    </div>
  );
}

/**
 * onChainSupplyDecimals — the exponent for the live supply block. The
 * contract's own declared scale wins; on the contract-storage arm the API
 * omits it when the chain declares none, and substituting a default there
 * would publish a figure wrong by a power of ten, so the answer is null
 * (render base units). The flow and native arms carry no scale of their
 * own and use the asset's resolved decimals.
 */
export function onChainSupplyDecimals(
  supply: AssetSupply,
  assetDecimals: number | undefined,
): number | null {
  if (supply.decimals != null) return supply.decimals;
  if (supply.source === 'contract_storage_balances') return null;
  return assetDecimals ?? 7;
}

function formatSupply(raw: string | undefined, decimals: number | null) {
  if (decimals == null) return formatBaseUnits(raw, 0, 0);
  const n = scaleBaseUnits(raw, decimals);
  return n != null ? formatCompact(n) : '—';
}

// OnChainSupply renders the live decode-at-ingest supply (ADR-0034):
// Σmint − Σburn − Σclawback from the supply_flows lake, current to the
// latest ledger with no rollup refresh. This is the universal supply
// number available for every token (vs the ADR-0011 F2 fields below,
// which only exist for tracked assets). For native XLM the source is the
// ledger header's total_coins (no mint/burn breakdown); for a token with
// no event log it is the per-holder balances read from contract storage,
// which can only be a floor once state expiry archives a balance.
function OnChainSupply({
  env,
  assetDecimals,
}: {
  env: Envelope<AssetSupply>;
  assetDecimals: number | undefined;
}) {
  const data = env.data;
  const native = data.source === 'ledger_total_coins';
  const storage = data.source === 'contract_storage_balances';
  const decimals = onChainSupplyDecimals(data, assetDecimals);
  const floor = data.circulating_supply_lower_bound === true;
  const total = formatSupply(data.total_supply, decimals);
  return (
    <div className="border-up/30 bg-up-subtle/50 rounded-lg border p-3">
      <h3 className="text-up mb-2 flex flex-wrap items-center gap-2 text-xs font-semibold tracking-wider uppercase">
        On-chain supply (live)
        <FreshnessMarker flags={env.flags} />
      </h3>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Metric
          label={floor ? 'Total (at least)' : 'Total'}
          value={total !== '—' && floor ? `≥ ${total}` : total}
          sublabel={supplySublabel(data, decimals)}
        />
        {!native && !storage && decimals != null && (
          <>
            <Metric
              label="Minted"
              value={formatSupply(data.mint_total, decimals)}
            />
            <Metric
              label="Burned"
              value={formatSupply(data.burn_total, decimals)}
            />
            <Metric
              label="Clawed back"
              value={formatSupply(data.clawback_total, decimals)}
            />
          </>
        )}
      </div>
      {!native && !storage && decimals != null && (
        <div className="mt-3">
          <SupplyFlowsBar rows={buildSupplyFlowRows(data, decimals)} />
        </div>
      )}
      {floor && (
        <p className="text-warn-700 mt-2 text-xs">
          A floor, not the exact supply: only balances that are ledger entries
          right now are counted, and state expiry can archive a real balance out
          of view
          {data.supply_consistent === false
            ? ' — the contract itself reports more than is visible.'
            : '.'}
        </p>
      )}
      <p className="text-up/80 mt-2 text-[11px]">
        {supplyFootnote(data)}
        {data.as_of_ledger != null &&
          ` Fresh to ledger ${data.as_of_ledger.toLocaleString('en-US')}.`}
      </p>
    </div>
  );
}

function supplySublabel(data: AssetSupply, decimals: number | null): string {
  if (decimals == null) return 'base units — the contract declares no scale';
  if (data.source === 'ledger_total_coins') return 'ledger total_coins';
  if (data.source === 'contract_storage_balances') {
    return `${(data.balance_entries ?? 0).toLocaleString('en-US')} balances`;
  }
  return `${data.flow_count.toLocaleString('en-US')} flows`;
}

function supplyFootnote(data: AssetSupply): string {
  if (data.source === 'ledger_total_coins') {
    return 'Native XLM total from the ledger header — current to the latest ledger.';
  }
  if (data.source === 'contract_storage_balances') {
    return 'Σ per-holder balances read from the contract’s own storage — the token emits no supply events.';
  }
  return 'Σ mint − burn − clawback from the supply_flows lake (ADR-0034), current to the latest ledger — no refresh lag.';
}

function Metric({
  label,
  value,
  sublabel,
}: {
  label: string;
  value: string;
  sublabel?: string;
}) {
  return (
    <div className="border-line bg-surface rounded-lg border p-3">
      <div className="text-ink-muted text-xs tracking-wider uppercase">
        {label}
      </div>
      <div className="text-ink mt-1 font-mono text-xl font-semibold">
        {value}
      </div>
      {sublabel && (
        <div className="text-ink-muted mt-1 truncate font-mono text-[11px]">
          {sublabel}
        </div>
      )}
    </div>
  );
}

// parseSmallest converts a smallest-integer-unit decimal string
// (stroops for classic / native; contract-defined for SEP-41) to
// a number for display. Returns null when the string is missing
// or not finite. Display-only — never used for further arithmetic
// (AGENTS.md invariant #1: precision lives in the string).
function parseSmallest(
  s: string | null | undefined,
  decimals: number,
): number | null {
  if (s == null) return null;
  const n = Number(s);
  if (!Number.isFinite(n)) return null;
  return n / 10 ** decimals;
}

function formatUSD(s: string): string {
  const n = Number(s);
  if (!Number.isFinite(n)) return s;
  return `$${formatCompact(n)}`;
}
