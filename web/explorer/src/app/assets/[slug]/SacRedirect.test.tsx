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
    expect(sacRedirectHref(SUSD_SAC, SUSD)).toBe(`/assets/${SUSD}/`);
    expect(
      sacRedirectHref('CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA', 'XLM'),
    ).toBe('/assets/XLM/');
  });

  it('keeps a Soroban-native token and non-contract slugs where they are', () => {
    expect(sacRedirectHref(SUSD_SAC, SUSD_SAC)).toBeNull();
    expect(sacRedirectHref(SUSD_SAC, undefined)).toBeNull();
    expect(sacRedirectHref(SUSD, 'sUSD')).toBeNull();
    expect(sacRedirectHref('usdc', 'USDC-GA5Z')).toBeNull();
  });
});

describe('SacRedirect', () => {
  it('replaces the URL and shows only a placeholder', () => {
    const { container } = render(<SacRedirect href={`/assets/${SUSD}/`} />);
    expect(replace).toHaveBeenCalledWith(`/assets/${SUSD}/`);
    expect(container.textContent).toBe('');
  });
});
