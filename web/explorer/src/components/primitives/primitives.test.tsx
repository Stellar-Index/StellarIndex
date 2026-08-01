import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/react';

import {
  DirectionPill,
  RankBadge,
  Sparkline,
  MultiWindowDelta,
  StreakIndicator,
  AccelerationArrow,
} from '@/components/primitives';

// Domain atoms are props-only and deterministic (per the primitives barrel
// contract). Assert they render without throwing across their key branches —
// the redesign can restyle freely, but must not break these entry points.
describe('domain primitives — render without throwing', () => {
  it('DirectionPill: shows the value when present, still renders for no-data', () => {
    // AGT-06: deltaPct is already a percentage-point number (5 = +5%), not
    // a fraction — pin the exact rendered string so this test can only
    // pass under the correct (code-matching) interpretation. A stale
    // fraction-based reading of the same input (0.052 -> "+0.05%") would
    // fail this assertion, unlike the old /5/ regex, which matched either
    // interpretation's output and so proved nothing.
    const up = render(<DirectionPill deltaPct={5.2} />);
    expect(up.container.textContent).toBe('+5.20%');
    const none = render(<DirectionPill deltaPct={null} />);
    expect(none.container.firstChild).not.toBeNull();
  });

  it('RankBadge: moved and brand-new both render', () => {
    expect(render(<RankBadge delta={3} />).container.firstChild).not.toBeNull();
    expect(render(<RankBadge delta={0} isNew />).container.firstChild).not.toBeNull();
  });

  it('Sparkline draws an <svg> from its values', () => {
    const { container } = render(<Sparkline values={[1, 2, 1.5, 3, 2.8]} />);
    expect(container.querySelector('svg')).not.toBeNull();
  });

  it('Sparkline is exposed to AT as an img with its accessible name (W6-acc-2)', () => {
    // Every chart primitive (LineChart/CandleChart/DonutChart/Bars/DepthChart)
    // pairs role="img" with aria-label. Without the explicit role the <svg>'s
    // accessible name is not reliably surfaced — getByRole('img') can only
    // find it once role="img" is present, so this fails against the unfixed
    // (aria-label-only) svg.
    const { getByRole } = render(<Sparkline values={[1, 2, 1.5, 3, 2.8]} />);
    expect(
      getByRole('img', { name: /sparkline: 5 points, up overall/i }),
    ).toBeInTheDocument();
  });

  it('MultiWindowDelta renders its window labels', () => {
    const { container } = render(
      <MultiWindowDelta
        windows={[
          { label: '24h', deltaPct: 0.012 },
          { label: '7d', deltaPct: -0.031 },
        ]}
      />,
    );
    expect(container.textContent).toContain('24h');
    expect(container.textContent).toContain('7d');
  });

  it('StreakIndicator: streak and ath variants render', () => {
    expect(
      render(<StreakIndicator kind="streak" direction="up" days={5} />).container.firstChild,
    ).not.toBeNull();
    expect(
      render(<StreakIndicator kind="ath" at={new Date(0).toISOString()} />).container.firstChild,
    ).not.toBeNull();
  });

  it('AccelerationArrow renders a direction×acceleration glyph', () => {
    expect(
      render(<AccelerationArrow direction="up" acceleration="increasing" />).container.firstChild,
    ).not.toBeNull();
  });
});
