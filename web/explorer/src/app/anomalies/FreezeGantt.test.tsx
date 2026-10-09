import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/react';

import { FreezeGantt, ganttRows } from './FreezeGantt';

const NOW = Date.parse('2026-10-09T12:00:00Z');

describe('ganttRows', () => {
  it('spans the window from the earliest freeze to now and runs firing spans to now', () => {
    const g = ganttRows(
      [
        {
          asset_id: 'native',
          quote_id: 'fiat:USD',
          frozen_at: '2026-10-09T00:00:00Z',
          recovered_at: '2026-10-09T06:00:00Z',
        },
        {
          asset_id: 'crypto:BTC',
          quote_id: 'fiat:USD',
          frozen_at: '2026-10-09T06:00:00Z',
          firing: true,
        },
      ],
      NOW,
    );
    expect(g?.startMs).toBe(Date.parse('2026-10-09T00:00:00Z'));
    expect(g?.rows.map((r) => r.asset)).toEqual(['crypto:BTC', 'native']);
    expect(g?.rows[0].spans[0]).toMatchObject({
      leftPct: 50,
      widthPct: 50,
      firing: true,
    });
    expect(g?.rows[1].spans[0]).toMatchObject({ leftPct: 0, widthPct: 50 });
  });

  it('drops events with unparseable or inverted timestamps', () => {
    expect(
      ganttRows(
        [
          { asset_id: 'native', quote_id: 'fiat:USD', frozen_at: 'garbage' },
          {
            asset_id: 'native',
            quote_id: 'fiat:USD',
            frozen_at: '2026-10-09T06:00:00Z',
            recovered_at: '2026-10-09T05:00:00Z',
          },
        ],
        NOW,
      ),
    ).toBeNull();
  });
});

describe('FreezeGantt', () => {
  it('renders nothing without a plottable event', () => {
    const { container } = render(<FreezeGantt events={[]} nowMs={NOW} />);
    expect(container.firstChild).toBeNull();
  });
});
