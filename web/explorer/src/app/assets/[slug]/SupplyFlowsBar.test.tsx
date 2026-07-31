import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { SupplyFlowsBar, buildSupplyFlowRows } from './SupplyFlowsBar';

describe('buildSupplyFlowRows', () => {
  it('scales smallest-unit strings to whole units and keeps absent totals null', () => {
    const rows = buildSupplyFlowRows(
      { mint_total: '10000000000', burn_total: '0', clawback_total: null },
      7,
    );
    expect(rows[0]).toMatchObject({ label: 'Minted', value: 1000, direction: 'add' });
    expect(rows[1]).toMatchObject({ label: 'Burned', value: 0, direction: 'remove' });
    // null (unserved) stays null — distinct from a served real zero.
    expect(rows[2].value).toBeNull();
  });
});

describe('SupplyFlowsBar', () => {
  it('renders one labelled row per flow with values, and — for unserved', () => {
    render(
      <SupplyFlowsBar
        rows={buildSupplyFlowRows(
          { mint_total: '10000000000', burn_total: '2500000000', clawback_total: null },
          7,
        )}
      />,
    );
    expect(screen.getByText('Minted')).toBeInTheDocument();
    expect(screen.getByText('Burned')).toBeInTheDocument();
    expect(screen.getByText('Clawed back')).toBeInTheDocument();
    expect(screen.getByText('1K')).toBeInTheDocument();
    expect(screen.getByText('250')).toBeInTheDocument();
    expect(screen.getByText('—')).toBeInTheDocument();
  });

  it('renders nothing when no flow is positive (nothing to compare)', () => {
    const { container } = render(
      <SupplyFlowsBar
        rows={buildSupplyFlowRows({ mint_total: null, burn_total: null, clawback_total: null }, 7)}
      />,
    );
    expect(container.firstChild).toBeNull();
  });
});
