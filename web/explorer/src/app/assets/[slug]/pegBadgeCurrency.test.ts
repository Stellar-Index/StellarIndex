// @vitest-environment node
//
// pegBadgeCurrency is the (code, issuer) gate in front of peggedTo — see
// AGENTS.md "ALWAYS key an asset on (code, issuer) … NEVER on code alone."
// peggedTo itself is a bare switch on the asset code string; an
// impersonator that claims a pegged ticker (e.g. a look-alike "USDC" whose
// issuer doesn't match the verified one) must NOT get the "Pegged to USD"
// badge, because that badge suppresses the change-pct pills that would
// otherwise show the impersonator's real, likely-wild price movement.
import { describe, it, expect, vi } from 'vitest';

// Same budget note as fetchPrice.test.ts: this file's cost is ONE
// `await import('./page')` resolving the whole page module graph.
vi.setConfig({ testTimeout: 30_000 });

describe('pegBadgeCurrency — AGENTS.md (code, issuer) rule', () => {
  it('badges the genuine verified asset (no unverified_warning)', async () => {
    const { pegBadgeCurrency } = await import('./page');
    expect(pegBadgeCurrency('USDC', undefined)).toBe('USD');
    expect(pegBadgeCurrency('USDC', null)).toBe('USD');
  });

  it('does NOT badge a code/issuer collision even though the code matches a peg', async () => {
    const { pegBadgeCurrency } = await import('./page');
    // The API's own issuer check: populated exactly when the code matches
    // a verified currency's ticker but the issuer does not — i.e. a
    // look-alike wearing the verified ticker.
    const collision = {
      verified_slug: 'usdc',
      verified_asset_id: 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
      verified_name: 'USD Coin',
      note: 'This asset shares a ticker with a verified currency but is not issued by it.',
    };
    expect(pegBadgeCurrency('USDC', collision)).toBeNull();
  });

  it('leaves unpegged codes alone regardless of warning presence', async () => {
    const { pegBadgeCurrency } = await import('./page');
    expect(pegBadgeCurrency('XLM', undefined)).toBeNull();
  });
});
