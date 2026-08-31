import { describe, it, expect } from 'vitest';

import { assetSlug } from './AssetLink';

// assetSlug decides where every asset reference in the explorer POINTS,
// and had no behavioural test — the repo-walk guard in
// lib/trust-surface-guards.test.ts pins the truncation IDIOM, but an
// idiom guard cannot tell you the function returns the right slug.
describe('assetSlug', () => {
  it('returns the full canonical id for a classic asset, not the bare code', () => {
    // The bare code is ambiguous — every USDC-alike shares /assets/USDC,
    // so a code-derived link can resolve to a different issuer's asset
    // than the row the user clicked (wave-D EXR-02). The canonical id is
    // pre-rendered for the same asset set, so this never links worse.
    expect(
      assetSlug('USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN'),
    ).toBe('USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN');
  });

  it('distinguishes two issuers sharing one code', () => {
    // The property that matters: same code, different issuer, different
    // link. Under the old code-truncating behaviour both collapsed to
    // 'USDC'.
    const real = assetSlug(
      'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
    );
    const impostor = assetSlug(
      'USDC-GBNLJIYH34FKMPBHQVBFNQXRTBTVJJTBHFJDVMPZPCJJDNKQEGDCVAAA',
    );
    expect(real).not.toBe(impostor);
  });

  it('maps native and its numeric alias to the native route', () => {
    expect(assetSlug('native')).toBe('native');
    expect(assetSlug('0')).toBe('native');
  });

  it('strips the fiat: and crypto: prefixes', () => {
    expect(assetSlug('fiat:USD')).toBe('USD');
    expect(assetSlug('crypto:BTC')).toBe('BTC');
  });

  it('refuses to link an unmapped oracle symbol', () => {
    // raw:<symbol> has no asset page by definition; linking it would
    // produce a static-export 404.
    expect(assetSlug('raw:XAUUSD')).toBeNull();
  });

  it('refuses to link a bare SAC contract id', () => {
    // Only linkable once resolved to its wrapped classic asset, which
    // AssetLink does via the SAC wrapper map.
    expect(
      assetSlug('CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA'),
    ).toBeNull();
  });

  it('refuses to link a canonical RWA asset', () => {
    // ADR-0028 rwa: ids have no /assets page: the API 400s on the bare
    // code and 404s on the prefixed id, so BOTH spellings are dead.
    // Returning null renders a plain label instead of promising a page
    // that does not exist (wave-D SI-OC-02).
    //
    // Stripping the prefix the way fiat:/crypto: do would produce
    // /assets/XAU and /assets/BENJI — which the API rejects — swapping
    // one dead link for another while destroying the namespace signal.
    for (const id of ['rwa:XAU', 'rwa:BENJI', 'rwa:USDY', 'rwa:XAUm']) {
      if (assetSlug(id) !== null) {
        throw new Error(
          `assetSlug(${id}) returned ${assetSlug(id)} — an rwa: asset must not be linked`,
        );
      }
    }
  });

  it('returns null for absent input', () => {
    expect(assetSlug(null)).toBeNull();
    expect(assetSlug(undefined)).toBeNull();
    expect(assetSlug('')).toBeNull();
  });
});
