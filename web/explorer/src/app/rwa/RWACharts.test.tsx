import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { PremiumBars, ShareBars } from './RWACharts';

describe('ShareBars', () => {
  it('ranks groups and marks a group with unvalued members as a floor', () => {
    render(
      <ShareBars
        ariaLabel="by class"
        rows={[
          { key: 'a', label: 'Bonds', usd: '100.00', unvalued: 0 },
          { key: 'b', label: 'Funds', usd: '900.00', unvalued: 2 },
          { key: 'c', label: 'Empty', usd: null, unvalued: 1 },
        ]}
      />,
    );
    const items = screen.getAllByRole('listitem');
    expect(items).toHaveLength(2);
    expect(items[0]).toHaveTextContent('Funds');
    expect(items[0]).toHaveTextContent('≥ $900');
    expect(items[0]).toHaveTextContent('2 unvalued');
    expect(items[1]).not.toHaveTextContent('≥');
  });

  it('shows a value above 2^53 without rounding', () => {
    render(
      <ShareBars
        ariaLabel="big"
        rows={[
          { key: 'a', label: 'Big', usd: '9007199254740993.01', unvalued: 0 },
        ]}
      />,
    );
    expect(screen.getByText('$9007.2T')).toBeInTheDocument();
    expect(screen.getByTitle('$9007199254740993.01')).toBeInTheDocument();
  });
});

it('names groups dropped for having no valuation', () => {
  render(
    <ShareBars
      ariaLabel="cap"
      rows={[
        { key: 'a', label: 'A', usd: '100', unvalued: 0 },
        { key: 'b', label: 'B', usd: null, unvalued: 3 },
      ]}
    />,
  );
  expect(screen.getByText('1 group with no valuation not shown')).toBeTruthy();
});

describe('PremiumBars', () => {
  it('draws published premiums only and labels both directions', () => {
    render(
      <PremiumBars
        assets={[
          {
            asset_id: 'A',
            code: 'AAA',
            premium: { status: 'published', pct: '0.5' },
          },
          {
            asset_id: 'B',
            code: 'BBB',
            premium: { status: 'published', pct: '-1.25' },
          },
          { asset_id: 'C', code: 'CCC', premium: { status: 'withheld' } },
        ]}
      />,
    );
    expect(
      screen.getByRole('img', { name: /premium or discount/i }),
    ).toBeInTheDocument();
    expect(
      screen.getByText('BBB — Trades below instrument value: 1.25%', {
        selector: 'title',
      }),
    ).toBeTruthy();
    expect(screen.queryByText(/CCC/)).toBeNull();
  });

  it('renders nothing with fewer than two published premiums', () => {
    const { container } = render(
      <PremiumBars
        assets={[
          { asset_id: 'C', code: 'CCC', premium: { status: 'withheld' } },
        ]}
      />,
    );
    expect(container.firstChild).toBeNull();
  });
});
