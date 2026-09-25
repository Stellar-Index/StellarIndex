import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

import { HBarList, PairedBars, DivergingColumns } from './Bars';
import { DonutChart } from './DonutChart';

describe('HBarList', () => {
  it('renders one labeled bar per item with the display value', () => {
    render(
      <HBarList
        ariaLabel="ops by type"
        items={[
          { label: 'payment', value: 120, display: '120' },
          {
            label: 'manage_offer',
            value: 40,
            display: '40',
            annotation: '(classic)',
          },
        ]}
      />,
    );
    // Each row must be exposed as its own accessible list item — role="img"
    // on the list would collapse every row's label/value/annotation into
    // the single aria-label string, per T280.
    expect(screen.queryByRole('img', { name: 'ops by type' })).toBeNull();
    expect(screen.getByRole('list', { name: 'ops by type' })).toBeInTheDocument();
    expect(screen.getAllByRole('listitem')).toHaveLength(2);
    expect(screen.getByText('payment')).toBeInTheDocument();
    expect(screen.getByText('120')).toBeInTheDocument();
    expect(screen.getByText('(classic)')).toBeInTheDocument();
  });

  it('renders nothing when every value is zero or non-finite — absence, not zero-claims', () => {
    const { container } = render(
      <HBarList
        ariaLabel="empty"
        items={[
          { label: 'a', value: 0 },
          { label: 'b', value: NaN },
        ]}
      />,
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
        rows={[
          { label: 'USDC', a: 100, b: 60, aDisplay: '$100', bDisplay: '$60' },
        ]}
      />,
    );
    // The per-row scale-bar list must stay a real list, not a single opaque
    // image, so each row's series values are individually reachable.
    expect(
      screen.queryByRole('img', { name: 'supplied vs borrowed' }),
    ).toBeNull();
    expect(
      screen.getByRole('list', { name: 'supplied vs borrowed' }),
    ).toBeInTheDocument();
    expect(screen.getByText('Supplied')).toBeInTheDocument();
    expect(screen.getByText('Borrowed')).toBeInTheDocument();
    expect(screen.getByText('$100')).toBeInTheDocument();
    expect(screen.getByText('$60')).toBeInTheDocument();
  });

  it('keeps a row whose half has no value, drawing that half as a dash, not a zero bar', () => {
    render(
      <PairedBars
        ariaLabel="supplied vs borrowed"
        aLabel="Supplied"
        bLabel="Borrowed"
        rows={[
          { label: 'USDC', a: 100, b: 60, aDisplay: '$100', bDisplay: '$60' },
          { label: 'EURC', a: 40, b: null, aDisplay: '$40' },
        ]}
      />,
    );
    const rows = screen.getAllByRole('listitem').filter((li) => li.textContent?.startsWith('EURC'));
    expect(rows).toHaveLength(1);
    expect(rows[0]).toHaveTextContent('$40');
    expect(rows[0]).toHaveTextContent('—');
    // one bar (Supplied) — the unpriced Borrowed half draws none
    expect(rows[0]!.querySelectorAll('span[aria-hidden]')).toHaveLength(1);
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
    expect(
      screen.getByRole('img', { name: 'movement flow' }),
    ).toBeInTheDocument();
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

// Two entries can share a display label — a real USDC and an impersonator
// both read "USDC" (AGENTS.md: code alone is an impersonation vector). The
// chart keys each entry by its id, so React never sees two siblings with
// one key and never reuses one asset's row for the other on a re-order.
describe('same-label entries keyed by id', () => {
  const USDC_A = 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
  const USDC_B = 'USDC-GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5';

  function duplicateKeyWarnings(run: () => void): unknown[][] {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
    try {
      run();
      return spy.mock.calls.filter((c) => c.some((a) => String(a).includes('same key')));
    } finally {
      spy.mockRestore();
    }
  }

  it('HBarList', () => {
    const items = [
      { id: USDC_A, label: 'USDC', value: 10, display: '10', title: USDC_A },
      { id: USDC_B, label: 'USDC', value: 5, display: '5', title: USDC_B },
    ];
    const warnings = duplicateKeyWarnings(() => {
      const { rerender } = render(<HBarList ariaLabel="held" items={items} />);
      rerender(<HBarList ariaLabel="held" items={[...items].reverse()} />);
    });
    expect(warnings).toEqual([]);
    expect(screen.getAllByRole('listitem').map((li) => li.getAttribute('title'))).toEqual([USDC_B, USDC_A]);
  });

  it('PairedBars', () => {
    const rows = [
      { id: USDC_A, label: 'USDC', a: 10, b: 5, title: USDC_A },
      { id: USDC_B, label: 'USDC', a: 4, b: 2, title: USDC_B },
    ];
    const warnings = duplicateKeyWarnings(() => {
      render(<PairedBars ariaLabel="pb" aLabel="a" bLabel="b" rows={rows} />);
    });
    expect(warnings).toEqual([]);
  });

  it('DivergingColumns', () => {
    const warnings = duplicateKeyWarnings(() => {
      render(
        <DivergingColumns
          ariaLabel="dc"
          posLabel="in"
          negLabel="out"
          buckets={[
            { id: USDC_A, label: 'USDC', pos: 3, neg: 1 },
            { id: USDC_B, label: 'USDC', pos: 2, neg: 1 },
          ]}
        />,
      );
    });
    expect(warnings).toEqual([]);
  });

  it('DonutChart', () => {
    const warnings = duplicateKeyWarnings(() => {
      render(
        <DonutChart
          data={[
            { id: USDC_A, label: 'USDC', value: 3 },
            { id: USDC_B, label: 'USDC', value: 2 },
          ]}
        />,
      );
    });
    expect(warnings).toEqual([]);
  });
});
