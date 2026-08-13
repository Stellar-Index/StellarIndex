import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';

import StatusPageClient from './StatusPageClient';

// EventSource doesn't exist under jsdom; the page opens one for the
// live ledger badge.
class FakeEventSource {
  static readonly CLOSED = 2;
  readyState = 0;
  onmessage: ((e: MessageEvent) => void) | null = null;
  onerror: ((e: Event) => void) | null = null;
  close() {
    this.readyState = 2;
  }
  addEventListener() {}
}

function statusPayload(overrides: Record<string, unknown>) {
  return {
    overall: 'ok',
    services: [],
    region: { name: 'r1', deployment: 'hetzner' },
    ...overrides,
  };
}

// Frontend-honesty sweep: the public status page coalesced absent
// measurements to zero — an unreachable latency backend rendered
// "0.0 ms" in green (a perfect-SLO claim from a missing measurement)
// and a failed freshness probe rendered "0 / 0" active sources (total
// ingest death). Absent must render '—'.
describe('StatusPageClient measurement tiles', () => {
  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  function mockStatus(payload: unknown) {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url.includes('/v1/status')) {
          return new Response(JSON.stringify({ data: payload }), {
            status: 200,
            headers: { 'content-type': 'application/json' },
          });
        }
        // Everything else (endpoint probes, per-region version +
        // ingestion snapshots) is left unreachable — this test is only
        // about the latency/freshness tiles.
        throw new Error('offline');
      }),
    );
  }

  it('renders — for latency and active sources when the measurements are absent', async () => {
    mockStatus(statusPayload({}));
    render(<StatusPageClient seedIncidents={[]} />);

    await waitFor(() => expect(screen.getByText('Request latency')).toBeInTheDocument());
    // Three "not measured" latency cells; no fabricated 0.0 ms.
    expect(screen.getAllByText('not measured').length).toBeGreaterThanOrEqual(3);
    expect(screen.queryByText('0.0')).not.toBeInTheDocument();
    // Active sources reads as unknown, not "0 / 0".
    expect(screen.queryByText('/ 0')).not.toBeInTheDocument();
  });

  it('renders the served measurements when they are present', async () => {
    mockStatus(
      statusPayload({
        latency: { window_secs: 300, p50_ms: 12.5, p95_ms: 40, p99_ms: 90 },
        freshness: { active_sources: 0, total_sources: 17 },
      }),
    );
    render(<StatusPageClient seedIncidents={[]} />);

    await waitFor(() => expect(screen.getByText('12.5')).toBeInTheDocument());
    expect(screen.getByText('40.0')).toBeInTheDocument();
    expect(screen.queryByText('not measured')).not.toBeInTheDocument();
    // A SERVED zero stays a zero — "0 / 17" is a real (alarming) reading,
    // not an absence, and must not be flattened into an em-dash.
    expect(screen.getByText('/ 17')).toBeInTheDocument();
  });
});
