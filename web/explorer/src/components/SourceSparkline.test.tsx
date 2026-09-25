import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { SourceSparkline } from './SourceSparkline';

describe('SourceSparkline', () => {
  it('draws no bar for an hour with no parsable volume — a gap, not a zero hour', () => {
    const { container } = render(
      <SourceSparkline
        buckets={[
          { hour: '2026-09-25T00:00:00Z', volume_usd: '10' },
          { hour: '2026-09-25T01:00:00Z', volume_usd: '' },
          { hour: '2026-09-25T02:00:00Z', volume_usd: 'n/a' },
          { hour: '2026-09-25T03:00:00Z', volume_usd: '5' },
        ]}
      />,
    );
    expect(container.querySelectorAll('rect')).toHaveLength(2);
  });

  it('says there is no data, not "no vol", when no hour carries a volume', () => {
    render(
      <SourceSparkline
        buckets={[
          { hour: '2026-09-25T00:00:00Z', volume_usd: '' },
          { hour: '2026-09-25T01:00:00Z', volume_usd: 'n/a' },
        ]}
      />,
    );
    expect(screen.queryByText('no vol')).toBeNull();
    expect(screen.getByText('—')).toBeInTheDocument();
  });

  it('still says "no vol" for hours that really traded nothing', () => {
    render(
      <SourceSparkline
        buckets={[
          { hour: '2026-09-25T00:00:00Z', volume_usd: '0' },
          { hour: '2026-09-25T01:00:00Z', volume_usd: '0.00' },
        ]}
      />,
    );
    expect(screen.getByText('no vol')).toBeInTheDocument();
  });
});
