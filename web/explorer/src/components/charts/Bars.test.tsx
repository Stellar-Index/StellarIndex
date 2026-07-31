import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { HBarList, PairedBars, DivergingColumns } from './Bars';

describe('HBarList', () => {
  it('renders one labeled bar per item with the display value', () => {
    render(
      <HBarList
        ariaLabel="ops by type"
        items={[
          { label: 'payment', value: 120, display: '120' },
          { label: 'manage_offer', value: 40, display: '40', annotation: '(classic)' },
        ]}
      />,
    );
    expect(screen.getByRole('img', { name: 'ops by type' })).toBeInTheDocument();
    expect(screen.getByText('payment')).toBeInTheDocument();
    expect(screen.getByText('120')).toBeInTheDocument();
    expect(screen.getByText('(classic)')).toBeInTheDocument();
  });

  it('renders nothing when every value is zero or non-finite — absence, not zero-claims', () => {
    const { container } = render(
      <HBarList ariaLabel="empty" items={[{ label: 'a', value: 0 }, { label: 'b', value: NaN }]} />,
    );
    expect(container.firstChild).toBeNull();
  });
});

describe('PairedBars', () => {
  it('renders a legend naming both series plus per-row values', () => {
    render(
      <PairedBars
        ariaLabel="supplied vs borrowed"
        aLabel="Supplied"
        bLabel="Borrowed"
        rows={[{ label: 'USDC', a: 100, b: 60, aDisplay: '$100', bDisplay: '$60' }]}
      />,
    );
    expect(screen.getByText('Supplied')).toBeInTheDocument();
    expect(screen.getByText('Borrowed')).toBeInTheDocument();
    expect(screen.getByText('$100')).toBeInTheDocument();
    expect(screen.getByText('$60')).toBeInTheDocument();
  });
});

describe('DivergingColumns', () => {
  it('renders the diverging chart with direction legend and edge labels', () => {
    render(
      <DivergingColumns
        ariaLabel="movement flow"
        posLabel="Received"
        negLabel="Sent"
        buckets={[
          { label: 'Jul 1', pos: 3, neg: 1 },
          { label: 'Jul 2', pos: 0, neg: 2 },
        ]}
      />,
    );
    expect(screen.getByRole('img', { name: 'movement flow' })).toBeInTheDocument();
    expect(screen.getByText('Received')).toBeInTheDocument();
    expect(screen.getByText('Sent')).toBeInTheDocument();
    expect(screen.getByText('Jul 1')).toBeInTheDocument();
    expect(screen.getByText('Jul 2')).toBeInTheDocument();
  });

  it('renders nothing when all buckets are zero', () => {
    const { container } = render(
      <DivergingColumns
        ariaLabel="empty"
        posLabel="in"
        negLabel="out"
        buckets={[{ label: 'd', pos: 0, neg: 0 }]}
      />,
    );
    expect(container.firstChild).toBeNull();
  });
});
