import { describe, it, expect, vi } from 'vitest';
import { render } from '@testing-library/react';

const replace = vi.fn();
vi.mock('next/navigation', () => ({
  useRouter: () => ({ replace }),
}));

import { sacRedirectHref } from '@/lib/assetRoute';

import { SacRedirect } from './SacRedirect';

const SUSD_SAC = 'CDFZUVS5YNLXU7VENKOUDEOHCJGKQNVUBWD7KMN6E7ZROKPYPFLRUJFG';
const SUSD = 'sUSD-GCHW7CWI7GMIYQYFXMFJNJX5645XGWIINIAEQK3SABQO6CAYL5T7JYIH';

describe('sacRedirectHref', () => {
  it('sends a SAC that resolves to a classic asset to that asset’s page', () => {
    expect(
      sacRedirectHref(SUSD_SAC, { slug: SUSD, code: 'sUSD', asset_id: SUSD }),
    ).toBe(`/assets/${SUSD}/`);
    expect(
      sacRedirectHref('CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA', {
        slug: 'xlm',
        code: 'XLM',
        asset_id: 'native',
      }),
    ).toBe('/assets/xlm/');
  });

  // A NULL-slug row is served with slug = its bare code, which routes to the
  // verified asset holding that ticker.
  it('sends a look-alike’s SAC to its own (code, issuer) page, not the ticker', () => {
    const fake = 'USDC-GBADSCAMXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX';
    expect(
      sacRedirectHref(SUSD_SAC, { slug: 'USDC', code: 'USDC', asset_id: fake }),
    ).toBe(`/assets/${fake}/`);
  });

  it('keeps a Soroban-native token and non-contract slugs where they are', () => {
    expect(
      sacRedirectHref(SUSD_SAC, { slug: SUSD_SAC, code: 'sUSD', asset_id: SUSD_SAC }),
    ).toBeNull();
    expect(sacRedirectHref(SUSD_SAC, undefined)).toBeNull();
    expect(sacRedirectHref(SUSD, { slug: 'sUSD', code: 'sUSD', asset_id: SUSD })).toBeNull();
    expect(sacRedirectHref('usdc', { slug: 'USDC-GA5Z' })).toBeNull();
  });
});

describe('SacRedirect', () => {
  it('replaces the URL and shows only a placeholder', () => {
    const { container } = render(<SacRedirect href={`/assets/${SUSD}/`} />);
    expect(replace).toHaveBeenCalledWith(`/assets/${SUSD}/`);
    expect(container.textContent).toBe('');
  });
});
