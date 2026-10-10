import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import {
  CountSparkline,
  FeeHeadroomBar,
  OpsPerTxBars,
  OpTypeStrip,
  ResultDonut,
  UpgradeBadges,
  closeIntervals,
  feeUsage,
} from './ChainCharts';

describe('ChainCharts', () => {
  it('donut splits successful and failed', () => {
    render(<ResultDonut ok={9} failed={1} />);
    expect(screen.getByRole('img').getAttribute('aria-label')).toContain(
      'Failed 10.0%',
    );
  });

  it('buckets transactions by op count', () => {
    render(<OpsPerTxBars opCounts={[1, 1, 3, 30]} />);
    const rows = screen.getByLabelText('Transactions by operation count');
    expect(rows.textContent).toContain('1 op');
    expect(screen.getByText('1 op').parentElement?.textContent).toContain('2');
  });

  it('sparkline carries min and max in its label', () => {
    render(<CountSparkline values={[3, 9, 5]} label="Txs per ledger" />);
    expect(
      screen.getByLabelText(/Txs per ledger: 3 ledgers, min 3, max 9/),
    ).toBeTruthy();
  });

  it('close intervals run oldest first and skip sequence gaps', () => {
    const at = (sequence: number, sec: number) => ({
      sequence,
      close_time: new Date(Date.UTC(2026, 0, 1, 0, 0, sec)).toISOString(),
    });
    // Newest first, as /v1/ledgers serves them; 8 → 10 is not adjacent.
    expect(
      closeIntervals([at(11, 22), at(10, 17), at(8, 5), at(7, 0)]),
    ).toEqual([5, 5]);
    expect(
      closeIntervals([at(2, 6), { sequence: 1, close_time: 'x' }]),
    ).toEqual([]);
  });

  it('sparkline names its unit', () => {
    render(<CountSparkline values={[5, 6]} label="Close gaps" noun="closes" />);
    expect(
      screen.getByLabelText(/Close gaps: 2 closes, min 5, max 6/),
    ).toBeTruthy();
  });

  it('fee usage is exact above 2^53', () => {
    expect(feeUsage('9007199254740993', '18014398509481986')).toEqual({
      pctTenths: 500,
      headroom: '9007199254740993',
    });
    expect(feeUsage('1', '0')).toBeNull();
  });

  it('renders fee headroom', () => {
    render(<FeeHeadroomBar charged="100" max="400" format={(s) => s} />);
    expect(screen.getByText(/25.0% of max fee used · 300 XLM/)).toBeTruthy();
  });

  it('counts repeated op types', () => {
    render(
      <OpTypeStrip types={['payment', 'payment', 'invoke_host_function']} />,
    );
    expect(screen.getByText('payment ×2')).toBeTruthy();
  });

  it('renders upgrade badges with the day', () => {
    render(
      <UpgradeBadges markers={[{ time: 1767225600, label: 'protocol v26' }]} />,
    );
    expect(screen.getByText('protocol v26 · 2026-01-01')).toBeTruthy();
  });
});
