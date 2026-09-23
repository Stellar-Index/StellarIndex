import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

const { seen, recordSource } = vi.hoisted(() => {
  const seen: string[] = [];
  const recordSource = ({ source }: { source: string }) => {
    seen.push(source);
    return null;
  };
  return { seen, recordSource };
});
vi.mock('./SourceHealthPanel', () => ({ SourceHealthPanel: recordSource }));
vi.mock('@/app/dexes/[source]/SourceStatsPanel', () => ({
  SourceStatsPanel: recordSource,
}));
vi.mock('@/app/dexes/[source]/SourceTopChart', () => ({
  SourceTopChart: recordSource,
}));

import { SourcePathView } from './SourcePathView';

// The runtime fallback for a source registered after the last build (T291).
describe('SourcePathView', () => {
  it('renders every live panel for the name read from the URL', () => {
    seen.length = 0;
    window.history.pushState({}, '', '/sources/newvenue/');
    render(<SourcePathView />);
    expect(
      screen.getByRole('heading', { level: 1, name: 'newvenue' }),
    ).toBeInTheDocument();
    expect(seen).toEqual(['newvenue', 'newvenue', 'newvenue']);
  });

  it('does not throw on a malformed percent-escape segment', () => {
    window.history.pushState({}, '', '/sources/%ZZ');
    expect(() => render(<SourcePathView />)).not.toThrow();
  });
});
