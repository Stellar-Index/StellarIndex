import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { Panel } from './Panel';

// #335 F5 (WCAG 1.3.1 / axe heading-order). Panel — not Card — is what
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
    const h = screen.getByRole('heading', { level: 2, name: 'Divergence board' });
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
