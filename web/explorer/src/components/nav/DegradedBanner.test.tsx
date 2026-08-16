import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, act, waitFor } from '@testing-library/react';

import { DegradedBanner } from './DegradedBanner';

// health-check: the /v1/status poll used to swallow every fetch error with
// an empty catch, leaving `overall` frozen at whatever it last was
// ('unknown' on first load) — so a TOTAL outage (the status feed itself
// unreachable) rendered nothing at all, the one case this banner exists to
// catch. Two consecutive failed polls should now flip it to a visible
// "unreachable" state instead of staying invisible forever.
describe('DegradedBanner', () => {
  afterEach(() => {
    // Restore timers + the fetch spy so this file cannot pollute (or be
    // polluted by) the rest of the suite — the default-order flake this
    // test previously rode (cross-file fetch-mock leakage).
    vi.useRealTimers();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('surfaces an unreachable-status banner after repeated fetch failures instead of staying silent', async () => {
    vi.useFakeTimers(); // this case drives the poll interval; the render-settle cases use real timers
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

  function mockStatusOnce(data: Record<string, unknown>) {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ data }),
    } as Response);
  }

  function renderAndSettle() {
    // Real timers + a findBy/waitFor at each call site deterministically
    // await the async status fetch, rather than a fragile fixed microtask
    // count that flaked under full-suite load.
    render(<DegradedBanner />);
  }

  // W1.4 honesty residue: a failed Alertmanager query zeroes the counts
  // AND (server-side) trips backendErr → overall="degraded", so the
  // banner renders. Reading `active_count ?? 0` published "0 active
  // alerts" — a false all-clear while alerting was blind. incidents_status
  // = "unknown" must surface the blind spot instead.
  it('renders "alert status unknown", not "0 active alerts", when incidents_status is unknown', async () => {
    mockStatusOnce({
      overall: 'degraded',
      incidents_status: 'unknown',
      // counts zeroed by the failed query — must NOT be trusted
      incidents: { active_count: 0, page_count: 0 },
    });

    renderAndSettle();

    const banner = await screen.findByRole('status');
    await waitFor(() => expect(banner.textContent).toMatch(/alert status unknown/i));
    expect(banner.textContent).not.toMatch(/0 active alert/i);
  });

  // Other direction: when the trust signal says the query succeeded, the
  // real count is shown (and it is NOT the "unknown" string).
  it('renders the genuine active-alert count when incidents_status is degraded', async () => {
    mockStatusOnce({
      overall: 'degraded',
      incidents_status: 'degraded',
      incidents: {
        active_count: 3,
        page_count: 0,
        active: [{ name: 'ExporterLagHigh', severity: 'ticket' }],
      },
    });

    renderAndSettle();

    const banner = await screen.findByRole('status');
    await waitFor(() => expect(banner.textContent).toMatch(/3 active alerts/i));
    expect(banner.textContent).not.toMatch(/alert status unknown/i);
  });
});
