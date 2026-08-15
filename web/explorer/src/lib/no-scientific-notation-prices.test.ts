import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

// UXP-26: ~15 price render sites called `n.toExponential(...)` for
// sub-milli values, printing e.g. "$7.407e-4" for a low-value asset/pair
// while the canonical @/lib/format formatters (formatPriceSmall /
// formatSubunitPrice / formatPairPrice) render the SAME magnitude as a
// plain decimal ("0.0007407"). The operator deliberately removed
// scientific-notation price rendering on 2026-08-06 (see format.ts:28-34)
// because it "is not user-friendly"; these sites bypassed that single
// source. This guard fails if any of them re-introduces toExponential in
// a price context — the canonical formatters (whose behaviour is asserted
// directly in format.test.ts) are the only sanctioned path.
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
  '../app/assets/[slug]/AssetConverter.tsx',
  '../app/assets/[slug]/LiquidityTabPanel.tsx',
  '../app/external/assets/[slug]/page.tsx',
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
