import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { EndpointLatencyBars, SourceVolumeCharts } from './StatusCharts';

describe('EndpointLatencyBars', () => {
  it('ranks slowest first and drops unmeasured probes', () => {
    render(
      <EndpointLatencyBars
        samples={[
          { path: '/v1/a', latencyMs: 120, tone: 'ok' },
          { path: '/v1/b', latencyMs: 950, tone: 'warn' },
          { path: '/v1/c', latencyMs: -1, tone: 'bad' },
        ]}
      />,
    );
    const rows = screen.getAllByRole('listitem').map((li) => li.textContent);
    expect(rows).toHaveLength(2);
    expect(rows[0]).toContain('/v1/b');
    expect(rows[0]).toContain('950 ms');
  });
});

describe('SourceVolumeCharts', () => {
  const rows = [
    { name: 'sdex', class: 'exchange', volume_24h_usd: '2500000' },
    { name: 'soroswap', class: 'exchange', volume_24h_usd: '1000' },
    { name: 'band', class: 'oracle', volume_24h_usd: null },
    { name: 'off', class: 'oracle', enabled: false, volume_24h_usd: '9' },
  ];

  it('renders volume bars for enabled sources with volume and a class donut', () => {
    render(<SourceVolumeCharts rows={rows} />);
    const list = screen.getByLabelText(/24h USD volume by source/);
    expect(list.textContent).toContain('sdex');
    expect(list.textContent).toContain('$2.5M');
    expect(list.textContent).not.toContain('band');
    expect(list.textContent).not.toContain('off');
    expect(screen.getAllByRole('img').length).toBeGreaterThan(0);
  });
});
