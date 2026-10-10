import { describe, it, expect, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';

vi.mock('@/lib/export', async () => {
  const actual =
    await vi.importActual<typeof import('@/lib/export')>('@/lib/export');
  return { ...actual, downloadText: vi.fn() };
});

import { downloadText } from '@/lib/export';

import { Panel } from './Panel';

// WCAG 1.3.1 / axe heading-order. Panel — not Card — is what
// renders the section titles on /divergences, and it hardcoded <h3>. With
// the page's own <h1> above it and the reference cards' <h2> below, the
// live outline read h1 → h3 → h3 → h3 → h2: a skipped level.
describe('reveal/Panel heading rank', () => {
  it('defaults to h3 (a panel nested under a SectionHeader h2)', () => {
    render(<Panel title="Nested panel">body</Panel>);
    expect(
      screen.getByRole('heading', { level: 3, name: 'Nested panel' }),
    ).toBeInTheDocument();
  });

  it('renders h2 when the panel IS a top-level page section', () => {
    render(
      <Panel headingLevel={2} title="Divergence board">
        body
      </Panel>,
    );
    const h = screen.getByRole('heading', {
      level: 2,
      name: 'Divergence board',
    });
    expect(h.tagName).toBe('H2');
    // No stray h3 left behind, so the outline cannot skip.
    expect(screen.queryByRole('heading', { level: 3 })).not.toBeInTheDocument();
    // Semantic change only — the panel's visual type scale is untouched.
    expect(h).toHaveClass('text-sm', 'font-medium');
  });

  it('keeps the hint as prose, not a second heading', () => {
    render(
      <Panel headingLevel={2} title="Titled" hint="context line">
        body
      </Panel>,
    );
    expect(screen.getByText('context line').tagName).toBe('P');
    expect(screen.getAllByRole('heading')).toHaveLength(1);
  });
});

describe('reveal/Panel download', () => {
  it('exports the held rows as CSV with amounts untouched', () => {
    render(
      <Panel
        title="Pools"
        download={{
          name: 'pools',
          columns: ['pool', 'reserve'],
          rows: [{ pool: 'P1', reserve: '92233720368547758070.0000001' }],
        }}
      >
        body
      </Panel>,
    );
    fireEvent.click(screen.getByRole('button', { name: 'CSV' }));
    expect(downloadText).toHaveBeenCalledWith(
      'pools.csv',
      'text/csv;charset=utf-8',
      'pool,reserve\r\nP1,92233720368547758070.0000001\r\n',
    );
  });

  it('renders no download group when there are no rows', () => {
    render(
      <Panel title="Pools" download={{ name: 'p', columns: ['a'], rows: [] }}>
        body
      </Panel>,
    );
    expect(screen.queryByRole('group')).toBeNull();
  });
});
