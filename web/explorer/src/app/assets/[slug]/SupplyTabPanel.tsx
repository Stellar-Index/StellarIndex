'use client';

import dynamic from 'next/dynamic';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { Badge, Callout, Skeleton } from '@/components/ui';
import { PANEL_LABEL_CLASS } from '@/components/ui/Page';
import { apiGet, asExample } from '@/api/client';
import { useAsset, useAssetSupply, type AssetSupply } from '@/api/hooks';
import { FreshnessMarker } from '@/components/primitives';
import { formatBaseUnits, formatCompactUnits } from '@/lib/format';
import { type Envelope } from '../../explorer-shared';
import { SupplyFlowsBar, buildSupplyFlowRows } from './SupplyFlowsBar';
import { CurrencyDeclarations } from './CurrencyDeclarations';
import { SupplyComposition } from './SupplyComposition';

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
      >
        <Callout tone="bad">Failed to load supply data.</Callout>
      </Panel>
    );
  }

  if (asset.isLoading) {
    return (
      <Panel
        headingLevel={2}
        title="Supply"
        source={asExample('/v1/assets/{asset_id}', { asset_id: assetID })}
      >
        <div className="space-y-3" aria-busy="true">
          <span className="sr-only">Loading supply</span>
          <Skeleton className="h-4 w-3/5" aria-hidden />
          <Skeleton className="h-4 w-full" aria-hidden />
          <Skeleton className="h-4 w-4/5" aria-hidden />
        </div>
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
            ? 'No circulating/max breakdown for this asset yet — the live on-chain total above is sourced directly from mint/burn flows.'
            : 'No supply snapshot available for this asset. The supply observer may not have backfilled it yet.'}
        </p>
      ) : (
        <>
          <h3 className={`${PANEL_LABEL_CLASS} mb-3`}>Supply snapshot</h3>
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-3">
            <Metric
              label="Circulating"
              value={formatCompactUnits(a.circulating_supply, decimals)}
              sublabel={`In smallest unit: ${a.circulating_supply ?? '—'}`}
            />
            <Metric
              label="Total"
              value={formatCompactUnits(a.total_supply, decimals)}
              sublabel={a.is_unlimited ? 'Issuer asserts unbounded' : ''}
            />
            <Metric
              label="Max"
              value={formatCompactUnits(a.max_supply, decimals)}
              sublabel={
                a.is_unlimited === true
                  ? 'Unlimited'
                  : a.max_supply_basis === 'sep1_declared_max'
                    ? 'Issuer-declared in stellar.toml'
                    : ''
              }
            />
          </div>

          <SupplyComposition
            circulating={a.circulating_supply}
            total={a.total_supply}
            max={a.max_supply}
            decimals={decimals}
            maxDeclared={a.max_supply_basis === 'sep1_declared_max'}
            unlimited={a.is_unlimited === true}
            floor={
              a.supply_basis === 'classic_trustline_sum' ||
              a.supply_basis === 'contract_storage_balances' ||
              onchain.data?.data?.circulating_supply_lower_bound === true
            }
          />

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
            <p
              className="text-ink-muted text-xs"
              title="Supply policy that produced these numbers."
            >
              <span className="font-mono">supply_basis</span>: {a.supply_basis}
            </p>
          )}

          {(a.fixed_number || a.max_number || a.is_unlimited != null) && (
            <div className="border-line border-t pt-3 text-xs">
              <h3
                className={PANEL_LABEL_CLASS}
                title="What the issuer pledged in their stellar.toml, distinct from the live-ledger numbers above."
              >
                SEP-1 issuance declarations
              </h3>
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
      <CurrencyDeclarations asset={a} />
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
    <div className="border-line border-t pt-4">
      <h3 className={`${PANEL_LABEL_CLASS} mb-2`}>Market-cap timeline</h3>
      {q.isLoading && (
        <div className="h-[260px]" aria-busy="true">
          <span className="sr-only">Loading market-cap timeline</span>
          <Skeleton className="h-full w-full" aria-hidden />
        </div>
      )}
      {/* A failed /v1/chart must not fall into the empty state
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
  return formatCompactUnits(raw, decimals);
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
    <div className="border-line border-b pb-4">
      <h3
        className={`${PANEL_LABEL_CLASS} mb-3 flex flex-wrap items-center gap-2`}
      >
        On-chain supply (live)
        <FreshnessMarker flags={env.flags} />
      </h3>
      <div className="grid grid-cols-2 gap-3">
        <Metric
          label={
            native ? 'Ledger total_coins' : floor ? 'Total (at least)' : 'Total'
          }
          value={total !== '—' && floor ? `≥ ${total}` : total}
          sublabel={supplySublabel(data, decimals)}
        />
        {storage && data.archived_balance_entries != null && (
          <Metric
            label="Archived"
            value={formatSupply(data.archived_balance_total, decimals)}
            sublabel={`${data.archived_balance_entries.toLocaleString('en-US')} balances — TTL lapsed, still restorable`}
          />
        )}
      </div>
      {!native && !storage && decimals != null && (
        <div className="mt-3">
          <SupplyFlowsBar rows={buildSupplyFlowRows(data, decimals)} />
        </div>
      )}
      {floor && (
        <p className="mt-2">
          <Badge
            tone="warn"
            title={`A floor, not the exact supply: only balances that are ledger entries right now are counted, and state expiry can archive a real balance out of view${
              data.supply_consistent === false
                ? ' — the contract itself reports more than is visible.'
                : '.'
            }`}
          >
            ≥ floor
          </Badge>
        </p>
      )}
      <details className="text-ink-muted mt-3 text-xs">
        <summary className="cursor-pointer">
          {data.as_of_ledger != null
            ? `Fresh to ledger ${data.as_of_ledger.toLocaleString('en-US')}`
            : 'Basis'}
        </summary>
        {supplyFootnote(data)}
      </details>
    </div>
  );
}

function supplySublabel(data: AssetSupply, decimals: number | null): string {
  if (decimals == null) return 'base units — the contract declares no scale';
  if (data.source === 'ledger_total_coins') return 'includes burned XLM';
  if (data.source === 'contract_storage_balances') {
    return `${(data.balance_entries ?? 0).toLocaleString('en-US')} balances`;
  }
  return `${data.flow_count.toLocaleString('en-US')} flows`;
}

function supplyFootnote(data: AssetSupply): string {
  if (data.source === 'ledger_total_coins') {
    return 'The ledger header’s total_coins counter. XLM burned by sending it to an unspendable account (SDF’s 2019 burn) still counts here, so it exceeds the total supply.';
  }
  if (data.source === 'contract_storage_balances') {
    return 'Σ per-holder balances read from the contract’s own storage — the token emits no supply events.';
  }
  return 'Σ mint − burn − clawback from on-chain supply flows, current to the latest ledger — no refresh lag.';
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
    <div className="min-w-0">
      <div className={PANEL_LABEL_CLASS}>{label}</div>
      <div className="text-ink mt-1.5 font-mono text-xl font-semibold tracking-tight">
        {value}
      </div>
      {sublabel && (
        <div className="text-ink-muted mt-1 font-mono text-xs break-all">
          {sublabel}
        </div>
      )}
    </div>
  );
}

function formatUSD(s: string): string {
  const c = formatCompactUnits(s);
  return c === '—' ? s : `$${c}`;
}
