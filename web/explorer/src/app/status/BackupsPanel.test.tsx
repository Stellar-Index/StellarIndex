import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import BackupsPanel, { sloShare } from './BackupsPanel';

// The public status page must show backup freshness honestly: green only
// within SLO, red with the real age past it, grey "no data" when a source is
// absent, and "not trustworthy" when the API's own Prometheus reads failed.

function renderPanel() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <BackupsPanel />
    </QueryClientProvider>,
  );
}

function verdict(
  status: 'ok' | 'stale' | 'unknown',
  age: number | null,
  slo: number,
) {
  return { status, age_seconds: age, slo_seconds: slo };
}

const SLO = {
  full_seconds: 8 * 86_400,
  diff_seconds: 36 * 3_600,
  wal_seconds: 15 * 60,
  offsite_seconds: 8 * 86_400,
  drill_seconds: 35 * 86_400,
  snapshot_seconds: 36 * 3_600,
};

const okFreshness = {
  full: verdict('ok', 5 * 86_400, SLO.full_seconds),
  diff: verdict('ok', 10 * 3_600, SLO.diff_seconds),
  wal: verdict('ok', 74, SLO.wal_seconds),
  drill: verdict('ok', 27 * 86_400, SLO.drill_seconds),
  snapshot: verdict('ok', 8 * 3_600, SLO.snapshot_seconds),
};

const repo = (n: string, kind: string, ts: string) => ({
  repo: n,
  kind,
  last_backup_ts: ts,
  retention: null,
});

const postgres = (offsiteTs: string) => ({
  last_full: {
    ts: '2026-08-24T02:31:10Z',
    size_bytes: 412_316_860_416,
    repo: '1',
  },
  last_diff: { ts: '2026-08-29T02:04:41Z', size_bytes: null },
  wal_archive_max_age_seconds: 74,
  repos: [
    repo('1', 'local', '2026-08-29T02:00:03Z'),
    repo('2', 'offsite', offsiteTs),
  ],
});

const drill = (over: Record<string, unknown> = {}) => ({
  last_run_ts: '2026-08-02T04:00:12Z',
  last_success_ts: '2026-08-02T04:00:12Z',
  result: 'pass',
  failed_checks: 0,
  restored_backup_ts: null,
  duration_s: null,
  ...over,
});

const clickhouse = {
  schema_snapshot_last_ts: '2026-08-29T03:40:05Z',
  schema_snapshot_offsite_last_ts: null,
  zfs_snapshot_latest_ts: null,
  replica_lag_s: null,
};

function payload(overrides: Record<string, unknown> = {}) {
  return {
    source_status: 'ok',
    postgres: postgres('2026-08-17T02:00:01Z'),
    restore_drill: drill(),
    clickhouse,
    freshness: {
      ...okFreshness,
      // repo2's newest backup is 12 d old, past the 8 d off-site SLO.
      offsite: verdict('stale', 12 * 86_400 + 36_000, SLO.offsite_seconds),
      overall: 'stale',
    },
    slo: SLO,
    ...overrides,
  };
}

function mockBackups(body: unknown, status = 200) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes('/v1/diagnostics/backups')) {
        return new Response(JSON.stringify(body), {
          status,
          headers: { 'content-type': 'application/json' },
        });
      }
      throw new Error('offline');
    }),
  );
}

describe('BackupsPanel', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  async function show(data: unknown, ready: string | RegExp) {
    mockBackups({ data, as_of: '2026-08-29T12:00:00Z' });
    renderPanel();
    await waitFor(() => expect(screen.getByText(ready)).toBeInTheDocument());
  }
  const absent = (text: string | RegExp) =>
    expect(screen.queryByText(text)).not.toBeInTheDocument();

  it('renders each item with its API-judged age and colours the breached off-site copy red', async () => {
    await show(payload(), 'Last full backup');
    // Roll-up: one item is past its SLO, so the headline says so.
    expect(screen.getByText('SLO breached')).toBeInTheDocument();
    absent('all within SLO');

    // The off-site row is RED with its real age, not a green zero.
    expect(screen.getByText('12d ago')).toHaveClass('text-bad-700');
    expect(screen.getByText('beyond SLO')).toBeInTheDocument();
    expect(
      screen.getByText('newest backup 2026-08-17 02:00 UTC'),
    ).toBeInTheDocument();

    // Ages come from the API's age_seconds (not a client clock) with the SLO
    // they were judged against; full + off-site share the 8 d SLO.
    expect(screen.getByText('5d ago')).toBeInTheDocument();
    expect(screen.getAllByText('SLO ≤ 8d').length).toBe(2);
    expect(screen.getAllByText('within SLO').length).toBe(5);
    expect(screen.getByText('1m ago')).toBeInTheDocument();
    expect(screen.getByText('SLO ≤ 15m')).toBeInTheDocument();

    // Full-backup detail: date, size, repo.
    expect(
      screen.getByText('2026-08-24 02:31 UTC · 384.0 GiB · repo 1'),
    ).toBeInTheDocument();
    // Drill: pass badge + last-run date.
    expect(screen.getByText('last run passed')).toBeInTheDocument();
    expect(
      screen.getByText('last run 2026-08-02 04:00 UTC · passed'),
    ).toBeInTheDocument();
    // Reserved-null lake fields read "no data", never 0.
    expect(
      screen.getByText(/ZFS snapshot: no data · replica lag: no data/),
    ).toBeInTheDocument();
  });

  it('renders grey "no data" for absent sources and never a fresh zero', async () => {
    const unknown = (slo: number) => verdict('unknown', null, slo);
    await show(
      payload({
        postgres: {
          last_full: null,
          last_diff: null,
          wal_archive_max_age_seconds: null,
          repos: [],
        },
        restore_drill: drill({
          last_run_ts: null,
          last_success_ts: null,
          result: 'unknown',
          failed_checks: null,
        }),
        clickhouse: { ...clickhouse, schema_snapshot_last_ts: null },
        freshness: {
          full: unknown(SLO.full_seconds),
          diff: unknown(SLO.diff_seconds),
          wal: unknown(SLO.wal_seconds),
          offsite: unknown(SLO.offsite_seconds),
          drill: unknown(SLO.drill_seconds),
          snapshot: unknown(SLO.snapshot_seconds),
          overall: 'unknown',
        },
      }),
      'Last full backup',
    );
    expect(screen.getAllByText('no data').length).toBe(6);
    expect(screen.getByText('partial data')).toBeInTheDocument();
    absent('within SLO');
    absent(/0s ago/);
    expect(screen.getByText('no drill recorded')).toBeInTheDocument();
    expect(
      screen.getByText('no off-site repository reported'),
    ).toBeInTheDocument();
    expect(
      screen.getByText('archiver age not exported on this host'),
    ).toBeInTheDocument();
    // Every age cell is the em-dash, in the faint (grey) tone.
    for (const dash of screen.getAllByText('—')) {
      expect(dash).toHaveClass('text-ink-faint');
    }
  });

  // The API refuses to judge a stamp from the future (clock skew / corrupt
  // label): "unknown" carrying the RAW negative age. The panel must name that
  // cause rather than send an operator hunting a missing exporter.
  it('names a future-dated stamp instead of showing it as "no data" or a fresh zero', async () => {
    await show(
      payload({
        // 8 d 14 h AHEAD of as_of.
        postgres: postgres('2026-09-07T02:00:01Z'),
        freshness: {
          ...okFreshness,
          offsite: verdict('unknown', -741_601, SLO.offsite_seconds),
          overall: 'unknown',
        },
      }),
      'Off-site copy (S3, repo 2)',
    );
    expect(screen.getByText('stamp from the future')).toBeInTheDocument();
    absent('no data');
    absent('— ago');
    // Never green, and never a fresh zero.
    absent('all within SLO');
    expect(screen.getByText('partial data')).toBeInTheDocument();
    absent(/0s ago/);
    // The future date stays visible so the skew is measurable from the page.
    expect(
      screen.getByText('newest backup 2026-09-07 02:00 UTC'),
    ).toBeInTheDocument();
    // The repositories caption must not clamp the future stamp up into "0s ago".
    expect(
      screen.getByText(/repo 2 \(offsite\) dated in the future/),
    ).toBeInTheDocument();
  });

  it('marks verdicts untrustworthy when the API could not read Prometheus', async () => {
    await show(
      payload({ source_status: 'unknown' }),
      'source unknown · verdicts not trustworthy',
    );
    absent('SLO breached');
    absent('all within SLO');
  });

  it('shows a failed drill as red with its failed-check count', async () => {
    await show(
      payload({
        restore_drill: drill({
          last_success_ts: '2026-07-05T04:00:12Z',
          result: 'fail',
          failed_checks: 2,
        }),
      }),
      'last run failed',
    );
    expect(
      screen.getByText('last run 2026-08-02 04:00 UTC · FAILED (2 checks)'),
    ).toBeInTheDocument();
  });

  it('renders the honest absence state when the endpoint is unavailable (503)', async () => {
    mockBackups(
      {
        type: 'https://api.stellarindex.io/errors/backups-unavailable',
        title: 'Backup diagnostics not available',
        status: 503,
      },
      503,
    );
    renderPanel();

    await waitFor(() =>
      expect(
        screen.getByText(/Backup freshness unavailable:/),
      ).toBeInTheDocument(),
    );
    expect(
      screen.getByText(/absence of data, not an all-clear/),
    ).toBeInTheDocument();
    absent('within SLO');
  });
});

describe('sloShare', () => {
  it('is age over SLO, past 1 when breached', () => {
    expect(sloShare(verdict('ok', 3600, 7200), 7200)).toBe(0.5);
    expect(sloShare(verdict('stale', 14400, 7200), 7200)).toBe(2);
  });

  it('has no bar for an unmeasured or future-dated age', () => {
    expect(sloShare(verdict('unknown', null, 7200), 7200)).toBeNull();
    expect(sloShare(verdict('unknown', -30, 7200), 7200)).toBeNull();
  });
});
