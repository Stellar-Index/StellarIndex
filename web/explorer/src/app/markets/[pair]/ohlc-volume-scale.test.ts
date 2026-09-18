import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

// F096: the OHLC panel rendered "Quote vol" as
// `Number(ohlc.quote_volume) / 1e7`. The API serves that figure as a raw
// smallest-unit integer whose scale belongs to the VENUES that traded —
// 7 decimals on-chain, 8 on a CEX (see
// internal/sources/external/coinbase.externalAmountDecimals) — so the
// fixed divisor printed every Coinbase-quoted pair, crypto:XLM/fiat:USD
// included, at ten times the market's volume. The bar now states its own
// scale (`quote_volume_decimals`) and the page must read it.
//
// Source-text guard rather than a render test because the regression is a
// literal: a constant divisor reintroduced anywhere on this panel is the
// defect, whatever the surrounding JSX looks like. The served value is
// asserted directly in internal/api/v1/ohlc_volume_scale_test.go.
// node:path, not `new URL(rel, import.meta.url)`: the jsdom environment
// overrides the global URL and rejects relative resolution against a
// file: base ("The URL must be of scheme file").
const abs = resolve(dirname(fileURLToPath(import.meta.url)), 'page.tsx');
const src = readFileSync(abs, 'utf8');

describe('markets/[pair] OHLC volume scale', () => {
  it('does not divide a volume by a hardcoded power of ten', () => {
    expect(src).not.toMatch(/(?:base|quote)_volume\s*\)?\s*\/\s*1e\d+/);
    expect(src).not.toMatch(
      /(?:base|quote)_volume\s*\)?\s*\/\s*10\s*\*\*\s*\d/,
    );
  });

  it('reads the scale the bar states', () => {
    expect(src).toContain('quote_volume_decimals');
    expect(src).toMatch(/10\s*\*\*\s*decimals/);
  });

  it('renders no figure at all when the bar states no scale', () => {
    // Guessing a divisor is what the finding is about; an em-dash is the
    // honest answer for a response that omits the field.
    expect(src).toMatch(/decimals === undefined[\s\S]{0,160}return '—'/);
  });
});
