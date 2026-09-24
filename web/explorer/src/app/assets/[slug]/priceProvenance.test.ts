import { describe, it, expect } from 'vitest';

import { headlinePriceProvenance } from './priceProvenance';

// The static asset page captions the listing-cache price by its
// server-declared basis. A two-hop (price_basis "transitive") price
// was captioned 'listing', so LiveAssetPrice treated it as a direct
// market figure: wrong caption, and a /v1/price withheld verdict on
// the direct market blanked a route price whose own legs were gated.
describe('headlinePriceProvenance', () => {
  it('captions a transitive listing price as transitive', () => {
    expect(
      headlinePriceProvenance(null, {
        price_usd: '7768.93',
        price_basis: 'transitive',
      }),
    ).toBe('transitive');
  });

  it('captions a declared-peg listing price as declared_peg', () => {
    expect(
      headlinePriceProvenance(null, {
        price_usd: '1.08',
        price_basis: 'declared_peg',
      }),
    ).toBe('declared_peg');
  });

  it('captions a basis-less listing price as the listing snapshot', () => {
    expect(headlinePriceProvenance(null, { price_usd: '0.9998' })).toBe(
      'listing',
    );
  });

  it('refuses to caption a basis this build does not know', () => {
    expect(
      headlinePriceProvenance(null, {
        price_usd: '2',
        price_basis: 'future_basis' as unknown as 'transitive',
      }),
    ).toBeNull();
  });

  it('prefers the /v1/price VWAP over the listing basis', () => {
    expect(
      headlinePriceProvenance(
        { price: '7768.9' },
        { price_usd: '7768.93', price_basis: 'transitive' },
      ),
    ).toBe('vwap1m');
    expect(
      headlinePriceProvenance(
        { price: '0.1', flags: { triangulated: true } },
        { price_usd: '0.1' },
      ),
    ).toBe('triangulated');
  });

  it('has no caption when there is no price at all', () => {
    expect(headlinePriceProvenance(null, { price_usd: null })).toBeNull();
  });
});
