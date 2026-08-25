'use client';

import { Panel } from '@/components/reveal';
import { asExample } from '@/api/client';
import { MarketChart } from '@/components/charts/MarketChart';
import { LineChart } from '@/components/charts/LineChart';
import { useFiatUsdSeries } from '@/api/hooks';
import { shortAssetText } from '@/lib/asset-label';

// The verified Circle-issued USDC — the chart anchor quote.
export const USDC_ASSET_ID =
  'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

// chartQuoteFor picks the ONE quote an asset's price chart renders
// against (2026-08-05 operator decision): every chart is anchored to
// USDC — never XLM. An XLM-quoted chart re-denominates the asset in a
// floating unit (the source of the header-vs-chart confusion this
// replaced), and XLM-quoted thin markets were exactly the manipulated
// visuals of the 2026-08-04 incident. USDC ≈ $1 by construction, so a
// USDC-quoted chart reads as dollars while staying a REAL traded
// market rather than a synthetic blend.
//
// Three cases:
//   - USDC itself charts against fiat:USD — its actual dollar price,
//     which is the surface where a depeg becomes visible (a USDC/USDC
//     chart would be a flat 1.0 by definition).
//   - Native XLM + fiat assets chart against fiat:USD (the CEX-fed
//     combined USD series — deeper than any single on-chain pair, and
//     "never XLM" trivially holds).
//   - Everything else charts against the verified USDC asset.
//
// Exported for the unit test; no toggle — one honest quote per asset.
export function chartQuoteFor(assetID: string): {
  quote: string;
  label: string;
} {
  if (assetID === USDC_ASSET_ID) {
    return { quote: 'fiat:USD', label: 'USD' };
  }
  if (assetID === 'native' || assetID.startsWith('fiat:')) {
    return { quote: 'fiat:USD', label: 'USD' };
  }
  return { quote: USDC_ASSET_ID, label: 'USDC' };
}

/**
 * Chart tab for /assets/[slug]?tab=chart. Three real cases, so a blank
 * candle chart never renders where a price exists in another shape:
 *   - USDC: it IS the dollar reference every chart is quoted against, so
 *     a USDC/USD candle chart is empty by construction — show a reference
 *     panel, not an empty grid.
 *   - Fiat currencies (fiat:*): the OHLC candle path has no on-chain
 *     constituent to build from; only /v1/chart carries the fx series, so
 *     render that as a line.
 *   - Everything else (incl. native XLM): real OHLC candles + volume via
 *     the shared MarketChart, quoted per [chartQuoteFor].
 */
export function ChartPanel({ assetID }: { assetID: string }) {
  if (assetID === USDC_ASSET_ID) {
    return <UsdcReferencePanel />;
  }
  if (assetID.startsWith('fiat:')) {
    return <FiatUsdChartPanel assetID={assetID} />;
  }

  const { quote, label } = chartQuoteFor(assetID);
  return (
    <Panel
      title="Price chart"
      hint={`OHLC + volume · quoted in ${label}`}
      source={asExample('/v1/ohlc', {
        base: assetID,
        quote,
        interval: '15m',
        limit: 672,
      })}
      bodyClassName="space-y-3"
    >
      <MarketChart
        base={assetID}
        quote={quote}
        baseLabel={shortAssetText(assetID)}
        quoteLabel={label}
        height={420}
        liveTip
      />
    </Panel>
  );
}

/**
 * FiatUsdChartPanel — a fiat currency's USD price as a line over
 * /v1/chart (the OHLC candle path is empty for fiat:USD — see ChartPanel).
 */
function FiatUsdChartPanel({ assetID }: { assetID: string }) {
  const { data, isLoading } = useFiatUsdSeries(assetID);
  const hasSeries = !!data && data.length >= 2;
  return (
    <Panel
      title="Price chart"
      hint="USD price · daily · trailing week"
      source={asExample('/v1/chart', {
        asset: assetID,
        quote: 'fiat:USD',
        timeframe: '1w',
        granularity: '1d',
      })}
      bodyClassName="space-y-3"
    >
      {hasSeries ? (
        <LineChart
          data={data}
          height={340}
          area
          positive={data[data.length - 1].value >= data[0].value}
          ariaLabel={`${shortAssetText(assetID)} USD price, trailing week`}
        />
      ) : (
        <div className="text-ink-muted flex h-[340px] items-center justify-center text-sm">
          {isLoading ? 'Loading…' : 'No recent USD price series for this currency.'}
        </div>
      )}
    </Panel>
  );
}

/**
 * UsdcReferencePanel — USDC has no on-chain USDC/USD series (it is the
 * dollar reference), so rather than an empty candle chart, state that.
 */
function UsdcReferencePanel() {
  return (
    <Panel title="Price chart" hint="USD reference">
      <div className="flex flex-col items-center justify-center gap-3 py-14 text-center">
        <div className="text-ink font-mono text-4xl tracking-tight">≈ $1.00</div>
        <p className="text-ink-muted max-w-md text-sm">
          USDC is the dollar reference every other asset is charted against,
          so it has no USDC-denominated chart of its own. To watch for a
          depeg, see the{' '}
          <a href="/divergences" className="text-brand-600 hover:underline">
            divergence board
          </a>
          .
        </p>
      </div>
    </Panel>
  );
}
