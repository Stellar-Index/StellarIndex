import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';

import { AccountDefiProtocolMix } from './AccountDefiProtocolMix';
import { AccountsTopBars } from './AccountsTopBars';
import { IssuerAssetMix } from '../issuers/[g_strkey]/IssuerAssetMix';

const G1 = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const G2 = 'GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA';

describe('AccountsTopBars', () => {
  it('ranks accounts with exact shares of the listed total above 2^53', () => {
    render(
      <AccountsTopBars
        isNative={false}
        rows={[
          { account_id: G1, value: '9007199254740993' },
          { account_id: G2, value: '9007199254740993' },
        ]}
      />,
    );
    expect(screen.getByText(/Top 2 of 2 listed accounts/)).toBeInTheDocument();
    expect(
      screen.getByLabelText(/Top 2 of 2 listed accounts by USD value/),
    ).toBeInTheDocument();
    expect(screen.getAllByText('$9007.2T')).toHaveLength(2);
    expect(screen.getAllByText('50.0%')).toHaveLength(2);
  });

  it('labels native rankings in XLM', () => {
    render(
      <AccountsTopBars isNative rows={[{ account_id: G1, value: '1500.5' }]} />,
    );
    expect(screen.getByText('1.5K XLM')).toBeInTheDocument();
  });
});

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
