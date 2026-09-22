import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import MethodologyPage from './page';

// The closed-bucket section described "Three regions ingest
// independently" in the present tense. One region serves the API:
// /v1/status reports `region.name = r1`, and /sla, /company and
// /careers all say one region on the same site. ADR-0050's
// three-region active/active topology is ratified and scheduled after
// v1.0 — the invariant is what makes it safe, not a description of
// today's deployment.
describe('MethodologyPage closed-bucket section', () => {
  it('does not claim three regions ingest today', () => {
    render(<MethodologyPage />);
    expect(
      screen.queryByText(/Three regions ingest independently/i),
    ).not.toBeInTheDocument();
  });

  it('states the single live region and dates the multi-region plan', () => {
    render(<MethodologyPage />);
    const para = screen.getByText(/load-bearing invariant behind/i);
    expect(para).toHaveTextContent(/One region serves the API today/i);
    expect(para).toHaveTextContent(/ADR-0050/);
    expect(para).toHaveTextContent(/after v1\.0/i);
  });
});

// T276: the freeze-triggers list claimed a >50%-filtered-trades outlier
// check, an all-exchange-source-class-collapse trigger, a >=2-oracle
// cross-oracle-divergence trigger, and an operator "freeze a pair
// manually" control. None of that exists: mapFreezeReason
// (internal/storage/timescale/freeze_events.go) only ever labels a
// Phase-1 ActionFreeze 'outlier_storm' or a phase2 decision
// 'divergence', and stellarindex-ops registers only freeze-unfreeze
// (end an escalated freeze), never a freeze-start command.
describe('MethodologyPage freeze triggers', () => {
  it('does not claim triggers or an operator control that do not exist in code', () => {
    render(<MethodologyPage />);

    expect(screen.queryByText(/Source-class collapse/i)).toBeNull();
    expect(screen.queryByText(/Cross-oracle divergence/i)).toBeNull();
    expect(screen.queryByText(/Operator-triggered/i)).toBeNull();
    expect(
      screen.queryByText(/50% of trades in the window/i),
    ).toBeNull();
    expect(
      screen.queryByText(/freeze a pair manually/i),
    ).toBeNull();
  });

  it('describes the actual single-source and statistical-divergence triggers, and the unfreeze-only operator control', () => {
    render(<MethodologyPage />);

    expect(screen.getByText('Single-source deviation')).toBeTruthy();
    expect(screen.getByText('Statistical divergence')).toBeTruthy();
    expect(
      screen.getByText(/on-call cannot start one by hand/i),
    ).toBeTruthy();
    expect(
      screen.getByText(/stellarindex-ops freeze-unfreeze/i),
    ).toBeTruthy();
  });
});
