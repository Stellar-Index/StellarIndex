import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';

import { AccountDefiProtocolMix } from './AccountDefiProtocolMix';
import { IssuerAssetMix } from '../issuers/[g_strkey]/IssuerAssetMix';

describe('AccountDefiProtocolMix', () => {
  it('shows position counts per protocol with an Other bucket', () => {
    const counts = ['a', 'b', 'c', 'd', 'e', 'f', 'g'].map((p, i) => ({
      protocol: p,
      label: p.toUpperCase(),
      count: 10 - i,
    }));
    render(<AccountDefiProtocolMix counts={counts} />);
    const label = screen.getByRole('img').getAttribute('aria-label') ?? '';
    expect(label).toContain('A 20.4%');
    expect(label).toContain('Other (2)');
  });

  it('renders nothing for a single protocol', () => {
    const { container } = render(
      <AccountDefiProtocolMix
        counts={[{ protocol: 'blend', label: 'Blend', count: 3 }]}
      />,
    );
    expect(container).toBeEmptyDOMElement();
  });
});

describe('IssuerAssetMix', () => {
  it('splits market cap exactly and counts assets without one', () => {
    render(
      <IssuerAssetMix
        assets={[
          {
            id: 'A',
            code: 'AAA',
            href: '/a',
            marketCapUsd: '9007199254740993',
          },
          {
            id: 'B',
            code: 'BBB',
            href: '/b',
            marketCapUsd: '9007199254740993',
          },
          { id: 'C', code: 'CCC', href: '/c', marketCapUsd: null },
        ]}
      />,
    );
    expect(
      screen.getByText(/1 without a market cap excluded/),
    ).toBeInTheDocument();
    const label = screen.getByRole('img').getAttribute('aria-label') ?? '';
    expect(label).toContain('AAA 50.0%');
    expect(screen.getByText(/^≥ \$/)).toBeInTheDocument();
  });

  it('abstains with fewer than two priced assets', () => {
    const { container } = render(
      <IssuerAssetMix
        assets={[{ id: 'A', code: 'AAA', href: '/a', marketCapUsd: '5' }]}
      />,
    );
    expect(container).toBeEmptyDOMElement();
  });
});
