import { describe, it, expect } from 'vitest';
import { render, screen, within } from '@testing-library/react';

import SLAPage from './page';

// The availability objective in the targets table is the customer
// commitment (#487): the burn-rate alerts, the sla-probe default and
// the operator docs are all derived from it, and a Go test in
// internal/ops/chops pins those to this cell. This test pins the page
// to itself: the error-budget section restates the figure and the
// 30-day downtime allowance it implies, and both have to move with the
// cell rather than be edited by hand — the page once said 99.9 % here
// while the alerts enforced 99.99 %, and nothing could notice.

function renderPage() {
  render(<SLAPage />);
}

function publishedAvailabilityPct(): number {
  const row = screen.getByText('Availability', { selector: 'td' }).closest('tr');
  expect(row).not.toBeNull();
  const cells = within(row as HTMLTableRowElement).getAllByRole('cell');
  const objective = cells[1].textContent ?? '';
  const m = /^≥ (\d+\.\d+) %$/.exec(objective.trim());
  expect(m, `availability objective cell reads ${JSON.stringify(objective)}`).not.toBeNull();
  return Number((m as RegExpExecArray)[1]);
}

describe('/sla availability figure', () => {
  it('is stated as a NN.N % objective in the targets table', () => {
    renderPage();
    const pct = publishedAvailabilityPct();
    expect(pct).toBeGreaterThan(99);
    expect(pct).toBeLessThan(100);
  });

  it('is the figure the error-budget section explains', () => {
    renderPage();
    const pct = publishedAvailabilityPct();
    const section = document.getElementById('error-budget');
    expect(section).not.toBeNull();
    const text = (section as HTMLElement).textContent ?? '';
    expect(text).toContain(`What ${pct} % actually permits`);
    expect(text).toContain(`A ${pct} % monthly availability objective`);
  });

  it('implies the downtime allowance the error-budget section quotes', () => {
    renderPage();
    const pct = publishedAvailabilityPct();
    // 30 days × (1 − objective), rounded to whole seconds: 99.9 % is
    // 43 min 12 s; 99.99 % would be 4 min 19 s.
    const allowanceSeconds = Math.round(30 * 24 * 60 * 60 * (1 - pct / 100));
    const minutes = Math.floor(allowanceSeconds / 60);
    const seconds = allowanceSeconds % 60;
    const section = document.getElementById('error-budget') as HTMLElement;
    expect(section.textContent).toContain(`${minutes} minutes ${seconds} seconds`);
  });
});
