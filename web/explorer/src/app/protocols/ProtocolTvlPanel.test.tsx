import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { ProtocolTvlPanel } from './ProtocolTvlPanel';

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
