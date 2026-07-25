import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, act } from '@testing-library/react';

import { DegradedBanner } from './DegradedBanner';

// health-check: the /v1/status poll used to swallow every fetch error with
// an empty catch, leaving `overall` frozen at whatever it last was
// ('unknown' on first load) — so a TOTAL outage (the status feed itself
// unreachable) rendered nothing at all, the one case this banner exists to
// catch. Two consecutive failed polls should now flip it to a visible
// "unreachable" state instead of staying invisible forever.
describe('DegradedBanner', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it('surfaces an unreachable-status banner after repeated fetch failures instead of staying silent', async () => {
    vi.spyOn(globalThis, 'fetch').mockRejectedValue(new TypeError('Failed to fetch'));

    render(<DegradedBanner />);

    // First poll fails — still within tolerance, no banner yet (avoids a
    // single blip flashing an alarming banner).
    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.queryByRole('status')).not.toBeInTheDocument();

    // Second consecutive failure (one interval tick later) — now surfaced.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    const banner = screen.getByRole('status');
    expect(banner.textContent).toMatch(/unreachable/i);
  });
});
