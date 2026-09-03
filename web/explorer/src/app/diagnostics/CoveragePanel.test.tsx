// The ADR-0033 recognition census is not a source. It used to arrive
// as a 21st row in /v1/coverage's `sources[]`, where it read
// permanently incomplete BY CONSTRUCTION (it can only be clean if no
// un-indexed Soroban contract exists anywhere on the network), so the
// public headline was an unfixable "20/21".
//
// The API now reports it as its own top-level axis and the headline
// counts sources only. The direction that change moves the number is
// the flattering one, so the panel has to carry the other half: the
// census must be MORE visible here, not less. These tests pin that —
// the numbers, and the plain-language statement that this is a
// discovery backlog rather than missing data.
import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

import { CoveragePanel } from './CoveragePanel';

const useCoverage = vi.hoisted(() => vi.fn());
vi.mock('@/api/hooks', () => ({ useCoverage }));

const blend = {
  source: 'blend',
  complete: true,
  lake_complete: true,
  substrate_ok: true,
  recognition_ok: true,
  projection_ok: true,
  genesis_ledger: 51499546,
  watermark_ledger: 64234754,
  tip_ledger: 64234754,
  coverage_pct: 1,
  computed_at: '2026-09-02T07:41:40Z',
};

// r1's real census, 2026-09-02.
const recognition = {
  all_shapes_recognized: false,
  unrecognized_shapes: 23945,
  unrecognized_contracts: 4172,
  earliest_ledger: 50560486,
  scanned_from_ledger: 50457424,
  tip_ledger: 64234754,
  meaning: 'Event shapes on Soroban contracts that no indexed source owns …',
  detail:
    '23945 unrecognized shape(s) on 4172 unowned contract(s) (earliest ledger 50560486) — run verify-recognition',
  computed_at: '2026-09-02T07:41:40Z',
};

function renderWith(data: unknown) {
  useCoverage.mockReturnValue({ data, isLoading: false, error: null });
  return render(<CoveragePanel />);
}

describe('CoveragePanel', () => {
  it('renders the recognition census with its numbers, outside the source table', () => {
    renderWith({
      sources: [blend],
      recognition,
      complete_sources: 1,
      lake_complete_sources: 1,
      total_sources: 1,
      network: 'pubnet',
      not_applicable_sources: [],
    });

    // The headline counts sources only.
    expect(screen.getByText('1/1 served tier')).toBeInTheDocument();

    // …and the census is still on the page, with both counts and the
    // ledger it starts at.
    expect(screen.getByText('23,945')).toBeInTheDocument();
    expect(screen.getByText('4,172')).toBeInTheDocument();
    expect(screen.getByText(/#50,560,486/)).toBeInTheDocument();
    // Twice: the status badge and the plain-language paragraph.
    expect(screen.getAllByText(/discovery backlog/i)).toHaveLength(2);

    // The load-bearing disclaimer: this is not a data gap.
    expect(
      screen.getByText(/a discovery backlog, not missing data/i),
    ).toBeInTheDocument();

    // It must not be rendered as a source row.
    expect(screen.queryByRole('link', { name: 'recognition' })).toBeNull();
  });

  it('says the census is ABSENT, not clean, when the audit has published none', () => {
    renderWith({
      sources: [blend],
      recognition: null,
      complete_sources: 1,
      lake_complete_sources: 1,
      total_sources: 1,
      network: 'pubnet',
      not_applicable_sources: [],
    });

    expect(screen.getByText(/no census\s+published yet/i)).toBeInTheDocument();
    expect(
      screen.getByText(/not a clean result; it is an absent one/i),
    ).toBeInTheDocument();
  });

  it('omits the counts rather than rendering an unverified 0', () => {
    renderWith({
      sources: [blend],
      recognition: {
        ...recognition,
        unrecognized_shapes: undefined,
        unrecognized_contracts: undefined,
        detail: 'legacy row written before the census format existed',
      },
      complete_sources: 1,
      lake_complete_sources: 1,
      total_sources: 1,
      network: 'pubnet',
      not_applicable_sources: [],
    });

    expect(screen.getByText(/Census counts unavailable/i)).toBeInTheDocument();
    expect(screen.queryByText('0')).toBeNull();
    // The audit's own words survive even when the counts don't parse.
    expect(
      screen.getByText(/legacy row written before the census format existed/),
    ).toBeInTheDocument();
  });
});
