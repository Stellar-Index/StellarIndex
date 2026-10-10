import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import {
  ContractDailyBars,
  CounterpartyBars,
  EventMixDonut,
  ExportChips,
  InlineBar,
  ProtocolMixDonut,
  RegistryCharts,
  protocolSlices,
  UNATTRIBUTED,
} from './ContractCharts';

describe('ContractCharts', () => {
  it('InlineBar scales to the max', () => {
    render(<InlineBar value={50} max={100} label="50 events" />);
    const el = screen.getByRole('img', { name: '50 events' });
    expect((el.firstChild as HTMLElement).style.width).toBe('50%');
  });

  it('ProtocolMixDonut carries an explicit unattributed slice', () => {
    render(
      <ProtocolMixDonut
        rows={[
          { protocol: 'blend', events: 30 },
          { protocol: null, events: 70 },
        ]}
      />,
    );
    expect(screen.getByRole('img').getAttribute('aria-label')).toContain(
      'Unattributed 70.0%',
    );
  });

  it('ContractDailyBars renders one bar per active day', () => {
    render(
      <ContractDailyBars
        daily={[
          { date: '2026-10-01T00:00:00Z', active_ledgers: 5 },
          { date: '2026-10-02T00:00:00Z', active_ledgers: 0 },
          { date: '2026-10-03T00:00:00Z', active_ledgers: 9 },
        ]}
      />,
    );
    const svg = screen.getByRole('img', { name: /Active ledgers per day/ });
    expect(svg.querySelectorAll('rect')).toHaveLength(2);
  });

  it('EventMixDonut groups by topic and flags partial coverage', () => {
    render(
      <EventMixDonut
        events={[
          { topic_0: 'transfer' },
          { topic_0: 'transfer' },
          { topic_0: 'mint' },
        ]}
      />,
    );
    expect(screen.getByRole('img').getAttribute('aria-label')).toContain(
      'transfer 66.7%',
    );
    expect(screen.getByText(/Partial: only the 3 events/)).toBeTruthy();
  });

  it('RegistryCharts ranks protocols and donuts categories', () => {
    render(
      <RegistryCharts
        rows={[
          { name: 'blend', category: 'lending', contract_count: 9 },
          { name: 'soroswap', category: 'dex', contract_count: 3 },
        ]}
      />,
    );
    const list = screen.getByRole('list', {
      name: 'Registered contracts per protocol',
    });
    expect(list.querySelectorAll('li')).toHaveLength(2);
    expect(screen.getByRole('img').getAttribute('aria-label')).toContain(
      'lending 75.0%',
    );
  });

  it('CounterpartyBars shows shared txs and the lower-bound note', () => {
    render(
      <CounterpartyBars
        edges={[{ contract_id: 'CABCDEFGHIJ', shared_txs: 1234 }]}
      />,
    );
    expect(screen.getByText('1,234')).toBeTruthy();
    expect(screen.getByText(/lower bounds/)).toBeTruthy();
  });

  it('protocolSlices sums per protocol and pools untagged rows', () => {
    const slices = protocolSlices(
      [
        { protocol: 'blend', n: 2 },
        { protocol: 'blend', n: 3 },
        { protocol: null, n: 4 },
        { protocol: '', n: 1 },
      ],
      (r) => r.n,
    );
    expect(slices.map(({ label, value }) => [label, value])).toEqual([
      ['blend', 5],
      [UNATTRIBUTED, 5],
    ]);
    expect(slices[0].color).toBeUndefined();
    expect(slices[1].color).toBeDefined();
  });

  it('CounterpartyBars donuts shared txs by protocol', () => {
    render(
      <CounterpartyBars
        edges={[
          { contract_id: 'CAAAAAAAAAA', protocol: 'soroswap', shared_txs: 30 },
          { contract_id: 'CBBBBBBBBBB', shared_txs: 10 },
        ]}
      />,
    );
    const label = screen.getByRole('img').getAttribute('aria-label') ?? '';
    expect(label).toContain('soroswap 75.0%');
    expect(label).toContain('Unattributed 25.0%');
  });

  it('ExportChips renders a chip per export with its signature', () => {
    render(
      <ExportChips
        exports={[{ name: 'swap', params: ['i64'], results: ['i64'] }]}
      />,
    );
    expect(screen.getByText('swap').getAttribute('title')).toBe('(i64) → i64');
  });
});
