import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { AnalyticsStatusNote, BespokeUnavailable } from './AnalyticsStatusNote';

describe('AnalyticsStatusNote', () => {
  it('renders nothing for ok / absent status', () => {
    const { container: okC } = render(
      <AnalyticsStatusNote analytics={{ status: 'ok', as_of: '2026-07-31T00:00:00Z' }} />,
    );
    expect(okC).toBeEmptyDOMElement();
    const { container: noneC } = render(<AnalyticsStatusNote />);
    expect(noneC).toBeEmptyDOMElement();
  });

  it('says degradation — not zero activity — when unavailable', () => {
    render(<AnalyticsStatusNote analytics={{ status: 'unavailable' }} />);
    expect(screen.getByRole('status').textContent).toMatch(/temporarily unavailable/i);
    expect(screen.getByRole('status').textContent).toMatch(/not.*activity is zero/i);
  });

  it('surfaces staleness with the snapshot age when stale', () => {
    render(
      <AnalyticsStatusNote
        analytics={{ status: 'stale', as_of: new Date(Date.now() - 60_000).toISOString() }}
      />,
    );
    const note = screen.getByRole('status');
    expect(note.textContent).toMatch(/snapshot/i);
    expect(note.textContent).toMatch(/background refresh/i);
  });
});

describe('BespokeUnavailable', () => {
  it('names the degraded state explicitly', () => {
    render(<BespokeUnavailable />);
    expect(
      screen.getByText(/analytics suite is temporarily unavailable/i),
    ).toBeInTheDocument();
  });
});
