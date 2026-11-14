import { describe, expect, it } from 'vitest';
import { mergeDetailOverListing } from './mergeDetail';

describe('mergeDetailOverListing', () => {
  it('does not put back a dollar figure the detail withheld', () => {
    const listing = {
      asset_id: 'ABC-G…',
      code: 'ABC',
      price_usd: '1.25',
      market_cap_usd: '412000000.00',
      change_24h_pct: '3.1',
    };
    const detail = {
      asset_id: 'ABC-G…',
      market_cap_low_liquidity: true,
      markets_count: 1,
    };
    const merged = mergeDetailOverListing(listing, detail);
    expect(merged).toEqual({
      asset_id: 'ABC-G…',
      code: 'ABC',
      market_cap_low_liquidity: true,
      markets_count: 1,
    });
  });

  it('keeps the detail figure when the detail serves one', () => {
    const merged = mergeDetailOverListing(
      { code: 'ABC', price_usd: '1.25' },
      { price_usd: '1.30' },
    );
    expect(merged.price_usd).toBe('1.30');
  });
});
