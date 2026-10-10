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
  incidentDays,
  probeEndpoint,
  type IncidentHistoryEntry,
  type PublicEndpoint,
} from './StatusPageClient';

// jsdom has no EventSource, and the live-ledger effect can open one after a
// test's teardown; a file-wide fake survives vi.unstubAllGlobals().
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
beforeAll(() => {
  (globalThis as { EventSource?: unknown }).EventSource = FakeEventSource;
});
afterAll(() => {
  delete (globalThis as { EventSource?: unknown }).EventSource;
});
afterEach(() => {
  vi.unstubAllGlobals();
});

const now = (agoMs = 0) => new Date(Date.now() - agoMs).toISOString();
const HOUR = 3_600_000;

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

type Handler = () => Promise<Response>;
type Text = string | RegExp;
const present = (...ts: Text[]) => {
  for (const t of ts) expect(screen.getByText(t)).toBeInTheDocument();
};
const absent = (...ts: Text[]) => {
  for (const t of ts) expect(screen.queryByText(t)).not.toBeInTheDocument();
};

// '/v1/status/notices' contains '/v1/status', so the specific path goes first.
// Every unrouted fetch (probes, other regions) is left unreachable.
function mockFeeds(h: {
  status?: Handler;
  ingestion?: Handler;
  notices?: Handler;
}) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    const handler = url.includes('/v1/status/notices')
      ? h.notices
      : url.includes('/v1/diagnostics/ingestion')
        ? h.ingestion
        : url.includes('/v1/status')
          ? h.status
          : undefined;
    if (handler) return handler();
    throw new Error('offline');
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

const status =
  (overrides: Record<string, unknown> = {}) =>
  async () =>
    json({
      data: {
        overall: 'ok',
        services: [],
        region: { name: 'r1', deployment: 'hetzner' },
        ...overrides,
      },
      as_of: now(),
    });

const ingestion =
  (overrides: Record<string, unknown> = {}, stale = false) =>
  async () =>
    json({
      data: {
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
          last_snapshot_at: now(),
        },
        backfill_coverage_as_of: now(),
        backfill_coverage: [],
        sources: [],
        ...overrides,
      },
      as_of: now(),
      flags: { stale },
    });

function renderPage(seedIncidents: IncidentHistoryEntry[] = []) {
  const client = new QueryClient();
  const utils = render(
    <QueryClientProvider client={client}>
      <StatusPageClient seedIncidents={seedIncidents} />
    </QueryClientProvider>,
  );
  return { client, ...utils };
}

// Captures the page's 30 s poll callbacks so a second poll fires
// deterministically (fake timers fight testing-library's waitFor).
function capturePolls() {
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
  return () =>
    act(async () => {
      for (const poll of polls) poll();
    });
}

// An absent measurement renders as unknown, never as a perfect 0.0 ms or a
// dead "0 / 0"; a served zero stays a zero.
describe('StatusPageClient measurement tiles', () => {
  it('renders absent latency and sources as not measured, never as zero', async () => {
    mockFeeds({ status: status() });
    renderPage();

    await screen.findByText('Request latency');
    const unknown = screen.getAllByText('not measured');
    expect(unknown.length).toBeGreaterThanOrEqual(3);
    absent('0.0', '/ 0');
  });

  it('hides the source count when freshness_status is unknown', async () => {
    mockFeeds({
      status: status({
        freshness: { active_sources: 0, total_sources: 17 },
        freshness_status: 'unknown',
      }),
    });
    renderPage();

    await screen.findByText('Active sources');
    absent('/ 17');
  });

  it('renders served measurements, including a served zero', async () => {
    mockFeeds({
      status: status({
        latency: { window_secs: 300, p50_ms: 12.5, p95_ms: 40, p99_ms: 90 },
        freshness: { active_sources: 0, total_sources: 17 },
      }),
    });
    renderPage();

    await screen.findByText('12.5');
    present('40.0', '/ 17');
    absent('not measured');
  });
});

describe('StatusPageClient honest staleness', () => {
  it('honours flags.stale on the ingestion envelope: no green 0s lag from a degraded snapshot', async () => {
    mockFeeds({
      status: status(),
      ingestion: ingestion(
        {
          ledger: {
            latest_ledger: 0,
            lag_seconds: 0,
            markets_count_24h: 0,
            assets_indexed: 0,
          },
        },
        true,
      ),
    });
    renderPage();

    await screen.findByText('Lag from tip');
    absent('0s');
    screen.getAllByText(/server degraded/i);
  });

  // useStatus keeps the last snapshot through failed polls; the headline must not.
  it('degrades the headline and stops the Live pulse once the status feed is unreachable', async () => {
    let up = true;
    const ok = status();
    mockFeeds({
      status: () => (up ? ok() : Promise.reject(new Error('Failed to fetch'))),
    });
    const { client } = renderPage();

    await screen.findByText('All systems operational');
    present(/Live · refreshed every 30 s/);

    up = false;
    // Two consecutive failed polls: the shared DegradedBanner threshold.
    for (let i = 0; i < 2; i++) {
      await act(async () => {
        await client.refetchQueries({ queryKey: ['/v1/status'] });
      });
    }

    await screen.findByText('Status unknown');
    absent('All systems operational', /Live · refreshed every 30 s/);
    present(/last successful poll/i);
  });

  const coverageRow = (over: Record<string, unknown>) => ({
    source: 'sdex',
    applies: true,
    genesis_ledger: 1,
    earliest_ledger: 1,
    latest_ledger: 100,
    entries: 5,
    ...over,
  });
  const verified = (computedAgo: number, snapshotAgo: number, over = {}) =>
    coverageRow({
      completeness_pct: 1,
      completeness_complete: true,
      completeness_lake_complete: true,
      completeness_computed_at: now(computedAgo),
      coverage_snapshot_at: now(snapshotAgo),
      ...over,
    });

  // The row is dated by its own verdict or scan, never the request's assembly
  // time; only a fresh, served-complete verdict keeps the green tone.
  it.each([
    [
      'marks a days-old verdict stale',
      verified(72 * HOUR, 72 * HOUR),
      [/3d ago/, /stale/],
      [],
      false,
    ],
    [
      'dates a verified row by its latest scan, not the daily verdict',
      verified(7 * HOUR, 5 * 60_000),
      [/5m ago/],
      [/7h ago/, /stale/],
      true,
    ],
    [
      'never labels the archive percentage as served while the served tier reconciles',
      verified(0, 0, {
        genesis_ledger: 2,
        earliest_ledger: 2,
        completeness_complete: false,
      }),
      [/archive complete/i, /served reconciling/i],
      [/%\s*served/],
      false,
    ],
  ])('%s', async (_, row, matches, misses, green) => {
    mockFeeds({
      status: status(),
      ingestion: ingestion({ backfill_coverage: [row] }),
    });
    renderPage();

    const tr = (await screen.findByText('sdex')).closest('tr')!;
    for (const m of matches) expect(tr.textContent).toMatch(m);
    for (const m of misses) expect(tr.textContent).not.toMatch(m);
    expect(tr.querySelector('.text-ok-700') !== null).toBe(green);
  });

  it('ages an unverified coverage row against its own scan cadence', async () => {
    const unverified = (source: string, cadenceS: number) =>
      coverageRow({
        source,
        gap_free_pct: 1,
        coverage_pct: 1,
        coverage_snapshot_at: now(3 * HOUR),
        coverage_scan_cadence_s: cadenceS,
      });
    mockFeeds({
      status: status(),
      ingestion: ingestion({
        backfill_coverage: [
          unverified('sdex', 6 * 3600),
          unverified('blend', 30 * 60),
        ],
      }),
    });
    renderPage();

    await screen.findByText('sdex');
    const ageCell = (source: string) =>
      screen
        .getByText(source)
        .closest('tr')!
        .querySelector<HTMLElement>('td[title^="When the gap detector"]')!;
    // 3 h into a 6 h scan cycle is healthy; 3 h on a 30 min cycle is not.
    expect(ageCell('sdex').className).not.toMatch(/text-warn-700/);
    expect(ageCell('sdex').title).toMatch(/6h cadence/);
    expect(ageCell('blend').className).toMatch(/text-warn-700/);
    expect(ageCell('blend').title).toMatch(/30m cadence/);
  });

  it('states no refresh cadence it cannot back in the empty state', async () => {
    mockFeeds({ status: status(), ingestion: ingestion() });
    renderPage();

    await screen.findByText(/Coverage snapshot pending/);
    absent(/every 5 min/);
  });

  // A notice exists for the outage window, so a failed poll keeps it, marked
  // stale. The store's read failure is a 200 with flags.stale and an empty
  // list, indistinguishable from "nothing to announce" without the flag.
  it.each([
    [
      'the notices feed throws',
      () => Promise.reject(new Error('Failed to fetch')),
    ],
    [
      'the notices feed answers 200 with flags.stale',
      async () =>
        json({ data: { notices: [], count: 0 }, flags: { stale: true } }),
    ],
  ])(
    'keeps the last operator notices with a stale marker when %s',
    async (_, failed) => {
      const firePolls = capturePolls();
      let up = true;
      const notice = {
        id: '11111111-1111-4111-8111-111111111111',
        title: 'API outage in progress',
        body: 'ETA 30 min',
        severity: 'major',
        status: 'active',
        created_at: now(),
      };
      mockFeeds({
        status: status(),
        notices: () =>
          up
            ? Promise.resolve(
                json({
                  data: { notices: [notice], count: 1 },
                  flags: { stale: false },
                }),
              )
            : failed(),
      });
      renderPage();

      await screen.findByText('API outage in progress');
      absent(/notices feed unreachable/i);

      up = false;
      await firePolls();

      present('API outage in progress', /notices feed unreachable/i);
    },
  );
});

// `overall: "ok"` beside open tickets is correct (a ticket never escalates the
// rollup); the banner names the backlog so both fields read as true.
describe('StatusPageClient overall banner ticket note', () => {
  const tickets = (active: number, page = 0) => ({
    active_count: active,
    page_count: page,
    ticket_count: active - page,
  });
  const OK = 'All systems operational';
  function renderWithIncidents(
    incidents: Record<string, unknown>,
    incidentsStatus: string,
    overall = 'ok',
  ) {
    mockFeeds({
      status: status({ overall, incidents, incidents_status: incidentsStatus }),
    });
    return renderPage();
  }

  it('names the open tickets beside the status word without changing the verdict', async () => {
    renderWithIncidents(tickets(8), 'degraded');

    const headline = await screen.findByText(OK);
    present('Operational');
    absent('Degraded performance');
    expect(headline.parentElement!.textContent).toContain('Operational');
    expect(headline.parentElement!.textContent).toContain('8 active tickets');
  });

  it('uses the singular for a single open ticket', async () => {
    renderWithIncidents(tickets(1), 'degraded');

    await screen.findByText('1 active ticket');
    absent('1 active tickets');
  });

  // Zero is silence; an "unknown" query makes any count fabricated whatever
  // it says; a page already moved `overall` and is not backlog.
  it.each([
    ['nothing is firing', tickets(0), 'ok', 'ok', OK],
    ['the alerting query failed', tickets(8), 'unknown', 'ok', OK],
    [
      'a page-severity alert is firing',
      tickets(9, 1),
      'degraded',
      'degraded',
      'Degraded performance',
    ],
  ])(
    'renders no ticket note when %s',
    async (_, incidents, incidentsStatus, overall, headline) => {
      renderWithIncidents(incidents, incidentsStatus, overall);

      await screen.findByText(headline);
      absent(/active ticket/i, /0 active/i);
    },
  );

  it('does not claim "No active incidents" when the alerting query failed', async () => {
    renderWithIncidents(tickets(0), 'unknown');

    await screen.findByText('Active incidents');
    present(/can.t confirm active incidents/i);
    absent('No active incidents.');
  });
});

// A 200 is not proof the API answered: a WAF page, a readyz reporting a
// non-critical failure, or an empty oracle set during an outage all return 200.
describe('probeEndpoint body-shape check', () => {
  const endpoint = (
    path: string,
    expectFn: (data: unknown) => boolean = () => true,
  ): PublicEndpoint => ({
    path,
    group: 'Health',
    description: path,
    probe: { kind: 'get', path, expect: expectFn },
  });
  const healthz = endpoint('/v1/healthz');
  const readyz = endpoint('/v1/readyz');
  const oracle = endpoint(
    '/v1/oracle/latest?asset=crypto:XLM',
    (data) => Array.isArray(data) && data.length > 0,
  );
  const envelope = (data: unknown) =>
    JSON.stringify({ data, as_of: '2026-01-01' });

  it.each([
    [
      'error',
      'a body that is not a v1 envelope',
      healthz,
      '<html>Attention Required! | Cloudflare</html>',
    ],
    ['fast', 'a genuine v1 envelope', healthz, envelope({ status: 'ok' })],
    [
      'degraded',
      'a readyz body reporting a non-critical failure',
      readyz,
      envelope({
        status: 'degraded',
        uptime: '3h12m4s',
        checks: [{ name: 'redis', ok: false, error: 'dial timeout' }],
      }),
    ],
    ['down', 'an oracle body with an empty reading set', oracle, envelope([])],
    [
      'fast',
      'an oracle body with a real reading',
      oracle,
      envelope([{ source: 'reflector', asset: 'crypto:XLM', price: '0.2' }]),
    ],
  ])('reports %s for a 200 with %s', async (kind, _, ep, body) => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response(body, { status: 200 })),
    );
    expect((await probeEndpoint(ep)()).kind).toBe(kind);
  });
});

describe('StatusPageClient header and incident history', () => {
  const seed = (entry: Partial<IncidentHistoryEntry>) => {
    mockFeeds({ status: status() });
    renderPage([
      {
        slug: 'x',
        date: '2026-09-19',
        title: 'partial pricing outage',
        resolved: '2026-09-19 12:00 UTC',
        summary: 'summary',
        severity: 'major',
        ...entry,
      } as IncidentHistoryEntry,
    ]);
  };

  // The link follows the `postmortem:` field, never the slug alone.
  it.each([
    [null, false],
    ['docs/operations/postmortems/2026-09-19-x.md', true],
  ])(
    'shows the postmortem link for postmortem=%j: %s',
    async (postmortem, shown) => {
      seed({ postmortem });
      await screen.findByText('partial pricing outage');
      await waitFor(() =>
        expect(screen.queryByText(/Read full postmortem/i) !== null).toBe(
          shown,
        ),
      );
    },
  );

  it('bounds the /v1/incidents request with an abort signal', async () => {
    seed({});
    await screen.findByText('partial pricing outage');
    const call = vi
      .mocked(globalThis.fetch)
      .mock.calls.find(([u]) => String(u).includes('/v1/incidents'));
    expect(call?.[1]?.signal).toBeInstanceOf(AbortSignal);
  });
});

describe('incidentDays', () => {
  it('buckets by start day, keeps the worst severity, drops out-of-window days', () => {
    const days = incidentDays(
      [
        { date: '2026-10-09', severity: 'minor' },
        { date: '2026-10-09', severity: 'major' },
        { date: '2026-10-10', severity: 'maintenance' },
        { date: '2026-10-01', severity: 'major' },
      ],
      '2026-10-10',
      3,
    );
    expect(days).toEqual([
      { day: '2026-10-08', severity: null, count: 0 },
      { day: '2026-10-09', severity: 'major', count: 2 },
      { day: '2026-10-10', severity: 'maintenance', count: 1 },
    ]);
  });

  it('returns nothing for an unparseable end day', () => {
    expect(incidentDays([], 'not-a-day')).toEqual([]);
  });
});
