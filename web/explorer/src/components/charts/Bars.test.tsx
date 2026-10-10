import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

import { HBarList, PairedBars, DivergingColumns } from './Bars';
import { DonutChart } from './DonutChart';

describe('charts render nothing when there is nothing to draw', () => {
  it.each([
    [
      'HBarList with zero or non-finite values',
      <HBarList
        key="h"
        ariaLabel="empty"
        items={[
          { label: 'a', value: 0 },
          { label: 'b', value: NaN },
        ]}
      />,
    ],
    [
      'DivergingColumns with all-zero buckets',
      <DivergingColumns
        key="d"
        ariaLabel="empty"
        posLabel="in"
        negLabel="out"
        buckets={[{ label: 'd', pos: 0, neg: 0 }]}
      />,
    ],
  ])('%s — absence, not zero-claims', (_name, chart) => {
    const { container } = render(chart);
    expect(container.firstChild).toBeNull();
  });
});

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
    // Rows must be list items; role="img" would collapse them into one label.
    expect(screen.queryByRole('img', { name: 'ops by type' })).toBeNull();
    expect(
      screen.getByRole('list', { name: 'ops by type' }),
    ).toBeInTheDocument();
    expect(screen.getAllByRole('listitem')).toHaveLength(2);
    expect(screen.getByText('payment')).toBeInTheDocument();
    expect(screen.getByText('120')).toBeInTheDocument();
    expect(screen.getByText('(classic)')).toBeInTheDocument();
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
    // A real list, not one opaque image, so each row's values stay reachable.
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
    const rows = screen
      .getAllByRole('listitem')
      .filter((li) => li.textContent?.startsWith('EURC'));
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
});

// A real USDC and an impersonator both read "USDC" (code alone is an
// impersonation vector), so charts must key entries by id, not label.
describe('same-label entries keyed by id', () => {
  const USDC_A =
    'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
  const USDC_B =
    'USDC-GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5';

  function duplicateKeyWarnings(run: () => void): unknown[][] {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
    try {
      run();
      return spy.mock.calls.filter((c) =>
        c.some((a) => String(a).includes('same key')),
      );
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
    expect(
      screen.getAllByRole('listitem').map((li) => li.getAttribute('title')),
    ).toEqual([USDC_B, USDC_A]);
  });

  it.each([
    [
      'PairedBars',
      <PairedBars
        key="p"
        ariaLabel="pb"
        aLabel="a"
        bLabel="b"
        rows={[
          { id: USDC_A, label: 'USDC', a: 10, b: 5, title: USDC_A },
          { id: USDC_B, label: 'USDC', a: 4, b: 2, title: USDC_B },
        ]}
      />,
    ],
    [
      'DivergingColumns',
      <DivergingColumns
        key="d"
        ariaLabel="dc"
        posLabel="in"
        negLabel="out"
        buckets={[
          { id: USDC_A, label: 'USDC', pos: 3, neg: 1 },
          { id: USDC_B, label: 'USDC', pos: 2, neg: 1 },
        ]}
      />,
    ],
    [
      'DonutChart',
      <DonutChart
        key="c"
        data={[
          { id: USDC_A, label: 'USDC', value: 3 },
          { id: USDC_B, label: 'USDC', value: 2 },
        ]}
      />,
    ],
  ])('%s', (_name, chart) => {
    expect(duplicateKeyWarnings(() => void render(chart))).toEqual([]);
  });
});
