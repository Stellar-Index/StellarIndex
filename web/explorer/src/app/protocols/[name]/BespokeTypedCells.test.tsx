import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { BespokeSection, BespokeTablePanel } from './BespokeSection';
import type { Bespoke, BespokeTable } from './BespokeSection';
import { donutSlices } from './BridgeShowcase';
import { asExample } from '@/api/client';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn(() => new Promise(() => {})) };
});

vi.mock('@/components/charts/LineChart', () => ({
  LineChart: (props: { series?: { label: string }[] }) => (
    <div
      data-testid="line-chart"
      data-series={(props.series ?? []).map((s) => s.label).join('|')}
    />
  ),
}));

const USDC_SAC = 'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75';
const POOL = 'C' + 'A'.repeat(54) + 'Q';
// A curated lending contract (LENDING_POOL_META) with no server label.
const CURATED = 'CDSYOAVXFY7SM5S64IZPPPYB4GVGGLMQVFREPSQQEZVIWXX5R23G4QSU';

function renderWithClient(ui: React.ReactElement) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>{ui}</QueryClientProvider>,
  );
}

const netFlow: BespokeTable = {
  title: 'Net flow by asset (30d)',
  columns: [
    'Asset',
    'Net supplied (supplied − withdrawn)',
    'Net borrowed (borrowed − repaid)',
    'Events',
  ],
  column_kinds: [
    { kind: 'asset' },
    { kind: 'amount', asset_column: 0 },
    { kind: 'amount', asset_column: 0 },
    { kind: 'count' },
  ],
  rows: [[USDC_SAC, '-12345678', '99', '42']],
  display: [['USDC', '-1.2345678', '', '']],
};

describe('typed bespoke cells', () => {
  it('renders the Blend net-flow table with asset codes and scaled amounts', () => {
    renderWithClient(
      <BespokeTablePanel
        table={netFlow}
        source={asExample('/v1/protocols/blend')}
      />,
    );
    const asset = screen.getByRole('link', { name: 'USDC' });
    expect(asset).toHaveAttribute('title', USDC_SAC);
    expect(asset).toHaveAttribute('href', `/contracts/${USDC_SAC}/`);
    expect(screen.queryByText(USDC_SAC)).toBeNull();
    const scaled = screen.getByText('-1.23');
    expect(scaled).toHaveAttribute(
      'title',
      '-1.2345678 (-12345678 base units)',
    );
    // No display: the raw base units stay, marked raw.
    expect(screen.getByText('raw')).toBeInTheDocument();
    expect(screen.getByText('42')).toBeInTheDocument();
  });

  it('names pools from the server label, else the curated lending name', () => {
    const table: BespokeTable = {
      title: 'Activity by pool',
      columns: ['Pool', 'Events'],
      column_kinds: [{ kind: 'pool' }, { kind: 'count' }],
      rows: [
        [POOL, '5'],
        [CURATED, '3'],
      ],
      display: [
        ['USDC / XLM', ''],
        ['', ''],
      ],
    };
    renderWithClient(
      <BespokeTablePanel
        table={table}
        source={asExample('/v1/protocols/blend')}
      />,
    );
    expect(screen.getByRole('link', { name: 'USDC / XLM' })).toHaveAttribute(
      'href',
      `/contracts/${POOL}/`,
    );
    const curated = screen.getByRole('link', { name: 'Pool Factory V2' });
    expect(curated).toHaveAttribute('href', `/lending/${CURATED}`);
    expect(curated).toHaveAttribute('title', CURATED);
  });

  it('keeps sniffing ids when a table carries no kinds', () => {
    const table: BespokeTable = {
      title: 'Legacy',
      columns: ['Pool'],
      rows: [[POOL]],
    };
    renderWithClient(
      <BespokeTablePanel
        table={table}
        source={asExample('/v1/protocols/comet')}
      />,
    );
    expect(screen.getByRole('link')).toHaveAttribute(
      'href',
      `/contracts/${POOL}/`,
    );
  });

  it('labels and links donut slices by the resolved entity', () => {
    const slices = donutSlices({
      title: 'Activity by pool',
      rows: [
        {
          label: 'CAAA…AAAQ',
          value: '10',
          count: 10,
          id: POOL,
          kind: 'pool',
          display: 'USDC / XLM',
        },
        { label: 'CDSY…4QSU', value: '5', count: 5, id: CURATED, kind: 'pool' },
        { label: 'supply', value: '2', count: 2 },
      ],
    });
    expect(slices.map((s) => [s.label, s.href])).toEqual([
      ['USDC / XLM', `/contracts/${POOL}/`],
      ['Pool Factory V2', `/lending/${CURATED}`],
      ['supply', undefined],
    ]);
  });

  it('legends grouped series by the resolved name without breaking the group', async () => {
    const pts = [{ date: '2026-07-27', value: '3' }];
    const bespoke: Bespoke = {
      category: 'lending',
      series: [
        {
          name: 'Pool · CAAA…AAAQ',
          unit: 'events',
          points: pts,
          id: POOL,
          kind: 'pool',
          display: 'USDC · XLM',
        },
        {
          name: 'Pool · CDSY…4QSU',
          unit: 'events',
          points: pts,
          id: CURATED,
          kind: 'pool',
        },
      ],
    };
    renderWithClient(
      <BespokeSection
        bespoke={bespoke}
        name="blend"
        source={asExample('/v1/protocols/blend')}
      />,
    );
    const charts = await screen.findAllByTestId('line-chart');
    expect(charts).toHaveLength(1);
    expect(charts[0]).toHaveAttribute(
      'data-series',
      'USDC · XLM|Pool Factory V2',
    );
  });
});
