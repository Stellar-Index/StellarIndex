import {
  describe,
  it,
  expect,
  vi,
  beforeAll,
  afterAll,
  afterEach,
} from 'vitest';
import { act, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import StatusPageClient, {
  probeEndpoint,
  type PublicEndpoint,
} from './StatusPageClient';

// The /v1/status doc now arrives via the SHARED useStatus query (FEC
// A6-6/D2 fold), so the page renders under a fresh QueryClient per case.
function renderPage() {
  const client = new QueryClient();
  return render(
    <QueryClientProvider client={client}>
      <StatusPageClient seedIncidents={[]} />
    </QueryClientProvider>,
  );
}

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
  // The live-ledger effect calls `new EventSource(...)` and can do so
  // after a test's teardown (React flushes the effect / the reconnect
  // timer fires post-test). A per-test stub that afterEach removes left
  // an "EventSource is not defined" race that surfaces under parallel
  // test load. Define the fake durably on globalThis for the whole file
  // so a late call always finds it; vi.unstubAllGlobals() (the per-test
  // fetch reset) does not touch a raw globalThis assignment.
  beforeAll(() => {
    (globalThis as { EventSource?: unknown }).EventSource = FakeEventSource;
  });
  afterAll(() => {
    delete (globalThis as { EventSource?: unknown }).EventSource;
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
    renderPage();

    await waitFor(() =>
      expect(screen.getByText('Request latency')).toBeInTheDocument(),
    );
    // Three "not measured" latency cells; no fabricated 0.0 ms.
    expect(screen.getAllByText('not measured').length).toBeGreaterThanOrEqual(
      3,
    );
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
    renderPage();

    await waitFor(() => expect(screen.getByText('12.5')).toBeInTheDocument());
    expect(screen.getByText('40.0')).toBeInTheDocument();
    expect(screen.queryByText('not measured')).not.toBeInTheDocument();
    // A SERVED zero stays a zero — "0 / 17" is a real (alarming) reading,
    // not an absence, and must not be flattened into an em-dash.
    expect(screen.getByText('/ 17')).toBeInTheDocument();
  });
});

// ---------------------------------------------------------------------------
// Honest-staleness sweep (audit 2026-08-28, web-status-1/2/4/6). The page
// rendered a server-flagged degraded ingestion snapshot as fresh green data,
// kept "All systems operational" + a pulsing "Live" while the status feed
// was unreachable, labelled days-old completeness verdicts with the
// request's assembly time, and blanked operator notices the moment the
// notices endpoint failed. Each case below fails on the pre-fix page.
// ---------------------------------------------------------------------------

type Handler = () => Promise<Response>;

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

// Route the page's fetches by path. Order matters: '/v1/status/notices'
// contains '/v1/status', so the more specific match is checked first.
function mockFeeds(h: {
  status?: Handler;
  ingestion?: Handler;
  notices?: Handler;
}) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url.includes('/v1/status/notices')) {
      if (h.notices) return h.notices();
      throw new Error('offline');
    }
    if (url.includes('/v1/diagnostics/ingestion')) {
      if (h.ingestion) return h.ingestion();
      throw new Error('offline');
    }
    if (url.includes('/v1/status')) {
      if (h.status) return h.status();
      throw new Error('offline');
    }
    throw new Error('offline');
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

function ingestionPayload(overrides: Record<string, unknown> = {}) {
  return {
    region: { name: 'r1', deployment: 'production' },
    version: {
      version: 'v0.47.2',
      build_date: '2026-08-28T00:00:00Z',
      commit: 'abcdef0123456789',
      dirty: 'false',
      go_version: 'go1.24',
    },
    ledger: {
      latest_ledger: 59_000_000,
      lag_seconds: 3,
      volume_24h_usd: '1000',
      markets_count_24h: 120,
      assets_indexed: 400,
    },
    fx_backfill: {
      earliest_quote: '2015-01-01',
      latest_quote: '2026-08-28',
      total_quotes: 10,
      currencies_count: 3,
    },
    supply: {
      classic_assets_with_supply: 1,
      sep41_assets_with_supply: 2,
      last_snapshot_at: new Date().toISOString(),
    },
    backfill_coverage: [],
    sources: [],
    ...overrides,
  };
}

function renderPageWithClient() {
  const client = new QueryClient();
  const utils = render(
    <QueryClientProvider client={client}>
      <StatusPageClient seedIncidents={[]} />
    </QueryClientProvider>,
  );
  return { client, ...utils };
}

describe('StatusPageClient honest staleness', () => {
  beforeAll(() => {
    (globalThis as { EventSource?: unknown }).EventSource = FakeEventSource;
  });
  afterAll(() => {
    delete (globalThis as { EventSource?: unknown }).EventSource;
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  // web-status-1: /v1/diagnostics/ingestion sets flags.stale=true when a
  // reader failed (zero-valued fields) or the refresher preserved a
  // last-known-good snapshot. The page dropped the flag and painted
  // "Lag from tip 0s" green with "Latest ledger 0".
  it('honours flags.stale on the ingestion envelope — no green 0s lag from a degraded snapshot', async () => {
    mockFeeds({
      status: async () => json({ data: statusPayload({}) }),
      ingestion: async () =>
        json({
          data: ingestionPayload({
            ledger: {
              latest_ledger: 0,
              lag_seconds: 0,
              markets_count_24h: 0,
              assets_indexed: 0,
            },
          }),
          as_of: new Date().toISOString(),
          flags: { stale: true },
        }),
    });
    renderPageWithClient();

    await waitFor(() =>
      expect(screen.getByText('Lag from tip')).toBeInTheDocument(),
    );
    // The failed measurement must not render as a perfect 0s lag …
    expect(screen.queryByText('0s')).not.toBeInTheDocument();
    // … and the server's degraded signal must be visible on the panel.
    expect(screen.getAllByText(/server degraded/i).length).toBeGreaterThan(0);
  });

  // web-status-2: useStatus keeps the last-known snapshot through failed
  // polls (by design), but the page derived its headline from it with no
  // regard for the poll error — "All systems operational" + a green
  // pulsing "Live" for as long as the tab stayed open during an outage.
  it('degrades the headline and stops the Live pulse once the status feed is unreachable', async () => {
    let up = true;
    mockFeeds({
      status: async () => {
        if (!up) throw new Error('Failed to fetch');
        return json({
          data: statusPayload({}),
          as_of: new Date().toISOString(),
        });
      },
    });
    const { client } = renderPageWithClient();

    await waitFor(() =>
      expect(screen.getByText('All systems operational')).toBeInTheDocument(),
    );
    expect(screen.getByText(/Live · refreshed every 30 s/)).toBeInTheDocument();

    up = false;
    // Two consecutive failed polls — the shared DegradedBanner threshold.
    await act(async () => {
      await client.refetchQueries({ queryKey: ['/v1/status'] });
    });
    await act(async () => {
      await client.refetchQueries({ queryKey: ['/v1/status'] });
    });

    await waitFor(() =>
      expect(screen.getByText('Status unknown')).toBeInTheDocument(),
    );
    expect(
      screen.queryByText('All systems operational'),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByText(/Live · refreshed every 30 s/),
    ).not.toBeInTheDocument();
    expect(screen.getByText(/last successful poll/i)).toBeInTheDocument();
  });

  // web-status-4: the coverage table's only age was backfill_coverage_as_of
  // — the API's per-request assembly time — while the completeness verdict
  // it displays can be days old. The per-row age must be rendered and an
  // aged verdict must not keep the green "verified" tone.
  it('renders the coverage data age per row and marks an aged verdict stale', async () => {
    const threeDaysAgo = new Date(Date.now() - 3 * 86_400_000).toISOString();
    mockFeeds({
      status: async () => json({ data: statusPayload({}) }),
      ingestion: async () =>
        json({
          data: ingestionPayload({
            backfill_coverage_as_of: new Date().toISOString(),
            backfill_coverage: [
              {
                source: 'sdex',
                applies: true,
                genesis_ledger: 1,
                earliest_ledger: 1,
                latest_ledger: 100,
                entries: 5,
                completeness_pct: 1,
                completeness_complete: true,
                completeness_lake_complete: true,
                completeness_computed_at: threeDaysAgo,
                coverage_snapshot_at: threeDaysAgo,
              },
            ],
          }),
          as_of: new Date().toISOString(),
          flags: { stale: false },
        }),
    });
    renderPageWithClient();

    await waitFor(() => expect(screen.getByText('sdex')).toBeInTheDocument());
    const row = screen.getByText('sdex').closest('tr');
    expect(row).not.toBeNull();
    // The verdict's real age, not the request's.
    expect(row!.textContent).toMatch(/3d ago/);
    expect(row!.textContent).toMatch(/stale/);
    // The green verified tone is withheld for an aged verdict.
    expect(row!.querySelector('.text-ok-700')).toBeNull();
  });

  it('states the real coverage refresh cadence in the empty state', async () => {
    mockFeeds({
      status: async () => json({ data: statusPayload({}) }),
      ingestion: async () =>
        json({
          data: ingestionPayload(),
          as_of: new Date().toISOString(),
          flags: { stale: false },
        }),
    });
    renderPageWithClient();

    await waitFor(() =>
      expect(screen.getByText(/Coverage snapshot pending/)).toBeInTheDocument(),
    );
    expect(screen.getByText(/every 30 min/)).toBeInTheDocument();
    expect(screen.queryByText(/every 5 min/)).not.toBeInTheDocument();
  });

  // web-status-6: an operator's "API outage in progress" notice vanished
  // from open tabs the moment the notices endpoint failed — the exact
  // window it exists for. Keep the last-known notices, marked stale.
  it('keeps the last operator notices with a stale marker when the notices feed fails', async () => {
    // Capture the page's 30 s poll callbacks so a second notices poll can be
    // fired deterministically (fake timers fight testing-library's waitFor).
    const polls: Array<() => void> = [];
    const realSetInterval = globalThis.setInterval;
    vi.stubGlobal('setInterval', ((
      fn: () => void,
      ms?: number,
      ...rest: unknown[]
    ) => {
      if (ms === 30_000) polls.push(fn);
      return realSetInterval(fn, ms, ...rest);
    }) as typeof setInterval);
    let up = true;
    mockFeeds({
      status: async () => json({ data: statusPayload({}) }),
      notices: async () => {
        if (!up) throw new Error('Failed to fetch');
        return json({
          data: {
            notices: [
              {
                id: '11111111-1111-4111-8111-111111111111',
                title: 'API outage in progress',
                body: 'ETA 30 min',
                severity: 'major',
                status: 'active',
                created_at: new Date().toISOString(),
              },
            ],
            count: 1,
          },
        });
      },
    });
    renderPageWithClient();

    await waitFor(() =>
      expect(screen.getByText('API outage in progress')).toBeInTheDocument(),
    );
    expect(
      screen.queryByText(/notices feed unreachable/i),
    ).not.toBeInTheDocument();

    up = false;
    await act(async () => {
      for (const poll of polls) poll();
    });

    // Still visible, now honestly marked as last-known.
    expect(screen.getByText('API outage in progress')).toBeInTheDocument();
    expect(screen.getByText(/notices feed unreachable/i)).toBeInTheDocument();
  });

  // RLT-465: a notice-store read failure returns HTTP 200 with an empty
  // list and flags.stale=true (status_notices.go handleStatusNotices) —
  // byte-identical on the surface to a genuine "nothing to announce". A
  // client that only checks res.ok would silently accept the empty list
  // as truth and clear whatever notice was previously showing.
  it('treats a 200 with flags.stale on the notices feed as a failed poll, not a genuine empty list', async () => {
    const polls: Array<() => void> = [];
    const realSetInterval = globalThis.setInterval;
    vi.stubGlobal('setInterval', ((
      fn: () => void,
      ms?: number,
      ...rest: unknown[]
    ) => {
      if (ms === 30_000) polls.push(fn);
      return realSetInterval(fn, ms, ...rest);
    }) as typeof setInterval);
    let degraded = false;
    mockFeeds({
      status: async () => json({ data: statusPayload({}) }),
      notices: async () =>
        json({
          data: degraded
            ? { notices: [], count: 0 }
            : {
                notices: [
                  {
                    id: '22222222-2222-4222-8222-222222222222',
                    title: 'Notice-store degraded',
                    body: 'investigating',
                    severity: 'major',
                    status: 'active',
                    created_at: new Date().toISOString(),
                  },
                ],
                count: 1,
              },
          flags: { stale: degraded },
        }),
    });
    renderPageWithClient();

    await waitFor(() =>
      expect(screen.getByText('Notice-store degraded')).toBeInTheDocument(),
    );

    degraded = true;
    await act(async () => {
      for (const poll of polls) poll();
    });

    // The stale read must not silently clear the banner …
    expect(screen.getByText('Notice-store degraded')).toBeInTheDocument();
    // … and must be flagged the same way an outright fetch failure is.
    expect(screen.getByText(/notices feed unreachable/i)).toBeInTheDocument();
  });
});

// ---------------------------------------------------------------------------
// Open-ticket note on the overall banner.
//
// /v1/status can serve `overall: "ok"` in the same body as
// `incidents.active_count: 8`. That is correct — rollupOverall escalates
// only on a service fault, a metrics-backend error, a breached latency SLO
// or a `page`-severity alert, and a ticket is none of those — but read
// together the two fields look like a contradiction. The banner names the
// backlog beside the status word so the reader can see why both are true.
// `overall` keeps its meaning; nothing here changes the verdict.
// ---------------------------------------------------------------------------
describe('StatusPageClient overall banner ticket note', () => {
  beforeAll(() => {
    (globalThis as { EventSource?: unknown }).EventSource = FakeEventSource;
  });
  afterAll(() => {
    delete (globalThis as { EventSource?: unknown }).EventSource;
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  function renderWithIncidents(
    incidents: Record<string, unknown>,
    incidentsStatus: string,
    overall = 'ok',
  ) {
    mockFeeds({
      status: async () =>
        json({
          data: statusPayload({
            overall,
            incidents,
            incidents_status: incidentsStatus,
          }),
          as_of: new Date().toISOString(),
        }),
    });
    return renderPageWithClient();
  }

  // The reported payload: overall "ok", eight firing tickets, no page.
  it('names the open tickets beside the status word', async () => {
    renderWithIncidents(
      { active_count: 8, page_count: 0, ticket_count: 8 },
      'degraded',
    );

    const headline = await screen.findByText('All systems operational');
    // The verdict is untouched — the note is a caption, not an escalation.
    expect(screen.getByText('Operational')).toBeInTheDocument();
    expect(screen.queryByText('Degraded performance')).not.toBeInTheDocument();
    // …and the count sits in the same row as the status word.
    const row = headline.parentElement;
    expect(row).not.toBeNull();
    expect(row!.textContent).toContain('Operational');
    expect(row!.textContent).toContain('8 active tickets');
  });

  it('uses the singular for a single open ticket', async () => {
    renderWithIncidents(
      { active_count: 1, page_count: 0, ticket_count: 1 },
      'degraded',
    );

    expect(await screen.findByText('1 active ticket')).toBeInTheDocument();
    expect(screen.queryByText('1 active tickets')).not.toBeInTheDocument();
  });

  // Zero is silence, not "0 active tickets" — a banner that announces an
  // empty backlog is noise on every healthy day.
  it('renders no note when nothing is firing', async () => {
    renderWithIncidents(
      { active_count: 0, page_count: 0, ticket_count: 0 },
      'ok',
    );

    await screen.findByText('All systems operational');
    expect(screen.queryByText(/active ticket/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/0 active/i)).not.toBeInTheDocument();
  });

  // incidents_status "unknown" means the Alertmanager query FAILED, so the
  // counts beside it are absence-of-signal (W1.1) — the trust signal is read
  // FIRST and the counts are not read at all. Carrying a non-zero count here
  // is the case that separates that rule from the zero check above: a note
  // sourced from an untrusted count is fabricated whatever the number says.
  it('renders no note when the alerting query failed', async () => {
    renderWithIncidents(
      { active_count: 8, page_count: 0, ticket_count: 8 },
      'unknown',
    );

    await screen.findByText('All systems operational');
    expect(screen.queryByText(/active ticket/i)).not.toBeInTheDocument();
  });

  // A page is not a ticket. It has already moved `overall` off "ok", so the
  // headline carries it — captioning it as backlog would both mislabel the
  // severity and soften a customer-facing fault.
  it('renders no ticket note while a page-severity alert is firing', async () => {
    renderWithIncidents(
      { active_count: 9, page_count: 1, ticket_count: 8 },
      'degraded',
      'degraded',
    );

    await screen.findByText('Degraded performance');
    expect(screen.queryByText(/active ticket/i)).not.toBeInTheDocument();
  });

  // RLT-465: incidents_status "unknown" means the Alertmanager query
  // FAILED, so `incidents.active` (empty here — the zero value of a
  // failed query) is absence-of-signal, not an all-clear. The panel used
  // to ignore incidents_status entirely and always render "No active
  // incidents." for an empty array, which is the exact silent collapse
  // W1.1 exists to prevent elsewhere on this page.
  it('does not claim "No active incidents" when the alerting query failed', async () => {
    renderWithIncidents(
      { active_count: 0, page_count: 0, ticket_count: 0 },
      'unknown',
    );

    await screen.findByText('Active incidents');
    expect(
      screen.getByText(/can.t confirm active incidents/i),
    ).toBeInTheDocument();
    expect(screen.queryByText('No active incidents.')).not.toBeInTheDocument();
  });
});

// RLT-385: a 2xx status alone isn't proof the API answered — a WAF
// challenge page, maintenance interstitial or misrouted edge response
// can return 200 with an unrelated body. probeEndpoint used to trust
// `res.ok` alone and report those as 'fast'.
describe('probeEndpoint body-shape check', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  const healthzEndpoint: PublicEndpoint = {
    path: '/v1/healthz',
    group: 'Health',
    description: 'Liveness probe',
    probe: { kind: 'get', path: '/v1/healthz', expect: () => true },
  };

  const readyzEndpoint: PublicEndpoint = {
    path: '/v1/readyz',
    group: 'Health',
    description: 'Readiness probe',
    probe: { kind: 'get', path: '/v1/readyz', expect: () => true },
  };

  // #784/#743: an endpoint that can answer 200 with an empty
  // collection during a total outage — Reflector wedged,
  // /v1/oracle/latest keeps answering 200 {"data":[]}.
  const oracleLatestEndpoint: PublicEndpoint = {
    path: '/v1/oracle/latest',
    group: 'Oracle',
    description: 'Latest oracle readings',
    probe: {
      kind: 'get',
      path: '/v1/oracle/latest?asset=crypto:XLM',
      expect: (data) => Array.isArray(data) && data.length > 0,
    },
  };

  it('reports error, not fast, for a 200 whose body is not a v1 envelope', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response('<html>Attention Required! | Cloudflare</html>', {
            status: 200,
            headers: { 'content-type': 'text/html' },
          }),
      ),
    );

    const result = await probeEndpoint(healthzEndpoint)();

    expect(result.kind).toBe('error');
  });

  it('reports fast for a genuine v1 envelope', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({ data: { status: 'ok' }, as_of: '2026-01-01' }),
            { status: 200, headers: { 'content-type': 'application/json' } },
          ),
      ),
    );

    const result = await probeEndpoint(healthzEndpoint)();

    expect(result.kind).toBe('fast');
  });

  // RLT-468: computeReadyz (server.go) returns HTTP 200 with
  // data.status="degraded" by design when a non-critical dependency
  // fails (F-1275) — 503 is reserved for a critical failure. Reading
  // `res.ok` alone can't distinguish that from a genuinely healthy
  // readyz, so the badge used to render a green "fast"/"slow" tick
  // for a degraded backend instead of a warning.
  it('reports degraded, not fast, for a 200 readyz body reporting a non-critical failure', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({
              data: {
                status: 'degraded',
                uptime: '3h12m4s',
                checks: [{ name: 'redis', ok: false, error: 'dial timeout' }],
              },
              as_of: '2026-01-01',
            }),
            { status: 200, headers: { 'content-type': 'application/json' } },
          ),
      ),
    );

    const result = await probeEndpoint(readyzEndpoint)();

    expect(result.kind).toBe('degraded');
  });

  // #784: a total oracle outage renders `200 {"data":[]}`, not a
  // non-2xx — a bare res.ok reports the row green through the outage.
  it('reports down, not fast, for a 200 oracle body with an empty reading set', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(JSON.stringify({ data: [], as_of: '2026-01-01' }), {
            status: 200,
            headers: { 'content-type': 'application/json' },
          }),
      ),
    );

    const result = await probeEndpoint(oracleLatestEndpoint)();

    expect(result.kind).toBe('down');
  });

  it('reports fast for a 200 oracle body with a real reading', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({
              data: [{ source: 'reflector', asset: 'crypto:XLM', price: '0.2' }],
              as_of: '2026-01-01',
            }),
            { status: 200, headers: { 'content-type': 'application/json' } },
          ),
      ),
    );

    const result = await probeEndpoint(oracleLatestEndpoint)();

    expect(result.kind).toBe('fast');
  });
});
