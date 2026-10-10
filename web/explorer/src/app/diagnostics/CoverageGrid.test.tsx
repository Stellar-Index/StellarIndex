import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';

import { CoverageGrid, coverageCells } from './CoverageGrid';

const verdict = {
  source: 'blend',
  complete: false,
  lake_complete: true,
  substrate_ok: true,
  recognition_ok: true,
  projection_ok: false,
  genesis_ledger: 1,
  watermark_ledger: 2,
  tip_ledger: 2,
  coverage_pct: 0.5,
  projection_evidenced_at: null,
  computed_at: '2026-09-02T07:41:40Z',
};

describe('coverageCells', () => {
  it('maps the three claims and two tiers in order', () => {
    expect(coverageCells(verdict)).toEqual([
      { axis: 'substrate', ok: true },
      { axis: 'recognition', ok: true },
      { axis: 'projection', ok: false },
      { axis: 'served', ok: false },
      { axis: 'lake', ok: true },
    ]);
  });

  it('reads a missing flag as failed', () => {
    const partial = { ...verdict, substrate_ok: undefined } as never;
    expect(coverageCells(partial)[0]).toEqual({ axis: 'substrate', ok: false });
  });
});

describe('CoverageGrid', () => {
  it('labels each failed cell', () => {
    render(<CoverageGrid sources={[verdict]} />);
    expect(screen.getByLabelText('Projection failed')).toBeTruthy();
    expect(screen.getByLabelText('Archive lake verified')).toBeTruthy();
  });
});
