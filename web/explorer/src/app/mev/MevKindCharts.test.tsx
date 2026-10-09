import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { MevKindCharts, dailyByKind } from './MevKindCharts';

const events = [
  { kind: 'arbitrage', detected_at: '2026-10-01T10:00:00Z' },
  { kind: 'arbitrage', detected_at: '2026-10-03T10:00:00Z' },
  { kind: 'sandwich', detected_at: '2026-10-03T11:00:00Z' },
];

describe('MevKindCharts', () => {
  it('keeps empty days and counts per kind', () => {
    const { days, byDay } = dailyByKind(events);
    expect(days).toEqual(['2026-10-01', '2026-10-02', '2026-10-03']);
    expect(byDay.get('2026-10-03').get('sandwich')).toBe(1);
    expect(byDay.get('2026-10-02')).toBeUndefined();
  });

  it('renders the by-kind bars and daily chart', () => {
    render(<MevKindCharts events={events} />);
    const list = screen.getByLabelText(/MEV events by kind, latest 3/);
    expect(list.textContent).toContain('arbitrage');
    expect(list.textContent).toContain('2');
    expect(screen.getByLabelText(/per UTC day by kind/)).toBeTruthy();
  });
});
