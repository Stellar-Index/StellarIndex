import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';

const __dirname = dirname(fileURLToPath(import.meta.url));

// Price render sites must not call `n.toExponential(...)` (prints "$7.407e-4");
// the canonical @/lib/format formatters (formatPriceSmall / formatSubunitPrice /
// formatPairPrice) render plain decimals ("0.0007407"). This guard fails if a
// price context re-introduces toExponential; the formatters (asserted in
// format.test.ts) are the only sanctioned path.
//
// Chart AXIS tick formatting is intentionally out of scope (a chart axis
// legitimately uses exponent labels): DepthChart.formatDepthPrice is the
// one deliberate exception and is not listed here.
const priceRenderFiles = [
  '../app/embed/pair/[pair]/page.tsx',
  '../app/embed/currency/[ticker]/page.tsx',
  '../app/embed/asset/[slug]/page.tsx',
  '../app/markets/[pair]/page.tsx',
  '../app/markets/[pair]/LivePairPrice.tsx',
  '../app/assets/[slug]/page.tsx',
  '../app/assets/[slug]/AssetSwap.tsx',
  '../app/assets/[slug]/LiquidityTabPanel.tsx',
  '../app/external/assets/[slug]/ExternalAssetDetailView.tsx',
  '../app/sources/[name]/page.tsx',
  '../app/accounts/AccountPositions.tsx',
];

describe.each(priceRenderFiles)('price render site %s', (rel) => {
  const abs = fileURLToPath(new URL(rel, import.meta.url));
  const src = readFileSync(abs, 'utf8');

  it('does not render prices with toExponential (uses @/lib/format decimals)', () => {
    expect(src).not.toContain('toExponential');
  });

  it('imports a canonical price formatter from @/lib/format', () => {
    expect(src).toMatch(/formatSubunitPrice|formatPairPrice|formatPriceSmall/);
    expect(src).toMatch(/from ['"]@\/lib\/format['"]/);
  });
});

// The Pages Function OG card renderer is not in the `../app/**`
// tree above and bundles standalone from it, so it can't import
// `@/lib/format` — but it renders the same sub-1 asset prices and must
// stay off both `toExponential` AND `toPrecision` (which itself drops
// into exponential notation below 1e-6, e.g. "7.407e-7").
describe('price render site ../../functions/og/[[path]].js', () => {
  const abs = resolve(__dirname, '../../functions/og/[[path]].js');
  const src = readFileSync(abs, 'utf8');

  it('does not render prices with toExponential or toPrecision', () => {
    expect(src).not.toContain('toExponential');
    expect(src).not.toContain('toPrecision');
  });
});
