'use client';

import { Panel } from '@/components/reveal';
import { Skeleton } from '@/components/ui';
import { asExample } from '@/api/client';
import { MarketChart } from '@/components/charts/MarketChart';
import { LineChart } from '@/components/charts/LineChart';
import { priceFormatForValues } from '@/components/charts/pricePrecision';
import { useFiatUsdSeries } from '@/api/hooks';
import { shortAssetText } from '@/lib/asset-label';
import { USDC_ASSET_ID } from '@/lib/networks';
import { withheldNote } from './headerNotes';
import { useScaleRefused } from './LiveAssetPrice';

// The verified Circle-issued USDC is the chart anchor quote.
export { USDC_ASSET_ID };

// chartQuoteFor picks the ONE quote an asset's price chart renders
// against: every chart is anchored to
// USDC — never XLM. An XLM-quoted chart re-denominates the asset in a
// floating unit (a source of header-vs-chart confusion), and
// XLM-quoted thin markets are exactly where manipulated visuals
// appear. USDC ≈ $1 by construction, so a
// USDC-quoted chart reads as dollars while staying a REAL traded
// market rather than a synthetic blend.
//
// Three cases:
//   - USDC itself maps to fiat:USD so the function never answers a
//     USDC/USDC quote (flat 1.0 by definition). ChartPanel renders a
//     reference panel for USDC and never calls this for it.
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
  if (
    assetID === 'native' ||
    assetID.startsWith('fiat:') ||
    assetID.startsWith('crypto:')
  ) {
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
export function ChartPanel({
  assetID,
  scaleMismatch,
}: {
  assetID: string;
  /** The asset row's decimals_mismatch verdict; null for an off-chain id, which has no token scale. */
  scaleMismatch: boolean | null;
}) {
  if (assetID === USDC_ASSET_ID) {
    return <UsdcReferencePanel />;
  }
  if (assetID.startsWith('fiat:')) {
    return <FiatUsdChartPanel assetID={assetID} />;
  }
  return <MarketPriceChart assetID={assetID} scaleMismatch={scaleMismatch} />;
}

function MarketPriceChart({
  assetID,
  scaleMismatch,
}: {
  assetID: string;
  scaleMismatch: boolean | null;
}) {
  // /v1/ohlc and the tip stream scale by the projection the asset row contradicts.
  const scaleRefused = useScaleRefused(
    scaleMismatch === null ? undefined : assetID,
    scaleMismatch ?? false,
  );
  if (scaleRefused) {
    return (
      <Panel
        headingLevel={2}
        title="Price chart"
        hint={withheldNote('decimals_mismatch')}
        source={asExample('/v1/assets/{id}', { id: assetID })}
      >
        <p className="text-ink-muted py-14 text-center text-sm">
          Price chart withheld: this token&apos;s on-chain decimals contradict
          the scale its market price is normalised on, so a per-unit price would
          be off by a power of ten.
        </p>
      </Panel>
    );
  }

  const { quote, label } = chartQuoteFor(assetID);
  return (
    <Panel
      headingLevel={2}
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
        volatilityBand
        sourceOverlay
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
      headingLevel={2}
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
          priceFormat={priceFormatForValues(data.map((p) => p.value))}
          positive={data[data.length - 1].value >= data[0].value}
          ariaLabel={`${shortAssetText(assetID)} USD price, trailing week`}
        />
      ) : (
        <div className="h-[340px]" aria-busy={isLoading || undefined}>
          {isLoading ? (
            <>
              <span className="sr-only">Loading price chart</span>
              <Skeleton className="h-full w-full" aria-hidden />
            </>
          ) : (
            <div className="text-ink-muted flex h-full items-center justify-center text-sm">
              No recent USD price series for this currency.
            </div>
          )}
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
    <Panel headingLevel={2} title="Price chart" hint="USD reference">
      <div className="flex flex-col items-center justify-center gap-3 py-14 text-center">
        {/* No constant price here: a fixed $1.00 would hide a depeg. */}
        <p className="text-ink-muted max-w-md text-sm">
          USDC is the dollar reference every other asset is charted against, so
          it has no USDC-denominated chart of its own. To watch for a depeg, see
          the{' '}
          <a href="/divergences" className="text-brand-600 hover:underline">
            divergence board
          </a>
          .
        </p>
      </div>
    </Panel>
  );
}
