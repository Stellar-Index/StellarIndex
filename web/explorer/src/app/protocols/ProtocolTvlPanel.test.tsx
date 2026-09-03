import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { ProtocolTvlPanel, type DexTvlTotal } from './ProtocolTvlPanel';

const AQUARIUS_TVL = {
  tvl_usd: '36553702.18',
  pools_total: 224,
  pools_priced: 74,
  unpriced_pools: 150,
  basis: 'sum of each pool: latest post-state reserve snapshot',
};

const COMET_TVL = {
  tvl_usd: '3652972.99',
  pools_total: 1,
  pools_priced: 1,
  unpriced_pools: 0,
  basis: 'sum of current per-token pool balance records',
};

describe('ProtocolTvlPanel', () => {
  it('renders one bar per protocol with TVL, lower-bound-marked when pools are unpriced', () => {
    render(
      <ProtocolTvlPanel
        rows={[
          { name: 'sdex' }, // no tvl → absent, never $0
          { name: 'aquarius', tvl: AQUARIUS_TVL },
          { name: 'comet', tvl: COMET_TVL },
        ]}
      />,
    );
    expect(screen.getByText('Value locked (USD)')).toBeInTheDocument();
    // Lower-bound prefix + priced/total split on the unpriced protocol…
    expect(screen.getByText('≥ $36.55M')).toBeInTheDocument();
    expect(screen.getByText('74/224 pools priced')).toBeInTheDocument();
    // …and a plain figure on the fully-priced one.
    expect(screen.getByText('$3.65M')).toBeInTheDocument();
    expect(screen.getByText(/lower bounds/)).toBeInTheDocument();
    // sdex has no TVL derivation — it must not appear at all.
    expect(screen.queryByText(/sdex/i)).not.toBeInTheDocument();
  });

  it('renders nothing when no row carries TVL (e.g. the bridges category)', () => {
    const { container } = render(
      <ProtocolTvlPanel rows={[{ name: 'cctp' }, { name: 'rozo', tvl: null }]} />,
    );
    expect(container.firstChild).toBeNull();
  });
});

// #338 — the headline total. The three states the wire can present, each
// of which has exactly one honest rendering.
const TOTAL: DexTvlTotal = {
  tvl_usd: '40206675.17',
  protocols: ['aquarius', 'comet'],
  lower_bound: true,
  pools_total: 225,
  pools_priced: 75,
  unpriced_pools: 150,
  as_of_ledger: 63_000_050,
  as_of: '2026-09-03T04:30:32Z',
  basis: 'exact sum of the published aquarius and comet figures',
  excluded: [
    { subject: 'classic liquidity pools', reason: 'CAP-38 pools are not valued yet' },
    { subject: 'blend', reason: 'lending supplied-value would flatter the headline' },
  ],
};

const ROWS = [
  { name: 'aquarius', tvl: AQUARIUS_TVL },
  { name: 'comet', tvl: COMET_TVL },
];

describe('ProtocolTvlPanel headline total', () => {
  it('renders a lower-bound total exactly, with ledger, basis and exclusions reachable', () => {
    render(<ProtocolTvlPanel rows={ROWS} total={TOTAL} />);
    expect(screen.getByText('Total value locked')).toBeInTheDocument();
    // The EXACT figure, grouped from the decimal string — not $40.21M,
    // and not a Number()-rounded approximation.
    expect(screen.getByText(/\$40,206,675\.17/)).toBeInTheDocument();
    // Lower bound: the "≥" mark the bars already use, plus the split.
    expect(screen.getByText('≥')).toBeInTheDocument();
    expect(screen.getByText(/75 of 225 pools priced/)).toBeInTheDocument();
    // Provenance on the surface, not buried in a hover.
    expect(
      screen.getByText(/exact sum of the published aquarius and comet figures/),
    ).toBeInTheDocument();
    expect(screen.getByText(/ledger 63,000,050/)).toBeInTheDocument();
    // Exclusions: one click away, and the count is in the summary.
    expect(screen.getByText('What this total excludes')).toBeInTheDocument();
    expect(screen.getByText('(2)')).toBeInTheDocument();
    expect(
      screen.getByText('CAP-38 pools are not valued yet'),
    ).toBeInTheDocument();
  });

  it('renders a fully-priced total without the lower-bound marks', () => {
    render(
      <ProtocolTvlPanel
        rows={ROWS}
        total={{ ...TOTAL, lower_bound: false, unpriced_pools: 0, pools_priced: 225 }}
      />,
    );
    expect(screen.getByText(/\$40,206,675\.17/)).toBeInTheDocument();
    expect(screen.queryByText('≥')).not.toBeInTheDocument();
    expect(screen.queryByText(/of 225 pools priced/)).not.toBeInTheDocument();
  });

  it('renders an ABSENT total as absent — never $0.00 and never a dash', () => {
    // tvl_total is omitempty: the server omits it when the reconciliation
    // could admit nothing. The bars still render (per-protocol figures are
    // still on the wire); the headline must not exist at all.
    render(<ProtocolTvlPanel rows={ROWS} total={undefined} />);
    expect(screen.getByText('Value locked (USD)')).toBeInTheDocument();
    expect(screen.queryByText('Total value locked')).not.toBeInTheDocument();
    expect(screen.queryByText(/\$0\.00/)).not.toBeInTheDocument();
    expect(screen.queryByText('—')).not.toBeInTheDocument();
    expect(screen.queryByText(/What this total excludes/)).not.toBeInTheDocument();
  });

  it('withholds the headline when a protocol it sums is not charted', () => {
    // The total is defined as the exact sum of the rows beside it. Filter
    // comet out (the /bridges + category-chip paths do exactly this) and
    // a headline of $40.2M no longer reconciles with the single visible
    // bar — so it is not shown rather than shown unreconcilable.
    render(
      <ProtocolTvlPanel rows={[{ name: 'aquarius', tvl: AQUARIUS_TVL }]} total={TOTAL} />,
    );
    expect(screen.getByText('Value locked (USD)')).toBeInTheDocument();
    expect(screen.queryByText('Total value locked')).not.toBeInTheDocument();
  });
});
