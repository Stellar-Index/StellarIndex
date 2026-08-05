'use client';

import { Panel } from '@/components/reveal';
import { asExample } from '@/api/client';
import { MarketChart } from '@/components/charts/MarketChart';

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
export function chartQuoteFor(assetID: string): { quote: string; label: string } {
  if (assetID === USDC_ASSET_ID) {
    return { quote: 'fiat:USD', label: 'USD' };
  }
  if (assetID === 'native' || assetID.startsWith('fiat:')) {
    return { quote: 'fiat:USD', label: 'USD' };
  }
  return { quote: USDC_ASSET_ID, label: 'USDC' };
}

// shortLabel renders a compact base label for the chart caption.
function shortLabel(assetID: string): string {
  if (assetID === 'native') return 'XLM';
  if (assetID.startsWith('fiat:')) return assetID.slice(5);
  const dash = assetID.indexOf('-');
  if (dash > 0) return assetID.slice(0, dash);
  if (assetID.length > 10) return `${assetID.slice(0, 4)}…${assetID.slice(-4)}`;
  return assetID;
}

/**
 * Chart tab for /assets/[slug]?tab=chart — real OHLC candles + volume
 * (the shared MarketChart over /v1/ohlc), quoted per [chartQuoteFor].
 */
export function ChartPanel({ assetID }: { assetID: string }) {
  const { quote, label } = chartQuoteFor(assetID);

  return (
    <Panel
      title="Price chart"
      hint={`OHLC + volume · quoted in ${label}`}
      source={asExample('/v1/ohlc', { base: assetID, quote, interval: '15m', limit: 672 })}
      bodyClassName="space-y-3"
    >
      <MarketChart
        base={assetID}
        quote={quote}
        baseLabel={shortLabel(assetID)}
        quoteLabel={label}
        height={420}
      />
    </Panel>
  );
}
