import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import BackupsPanel from './BackupsPanel';

// Backups panel (Ash, 2026-08-29): the public status page must show
// backup freshness honestly — green only within SLO, red with the real
// age past it, grey "no data" when a source is absent, and a
// "not trustworthy" marker when the API's own Prometheus reads failed.
// Each case asserts the CORRECTED rendering; the pre-panel page shows
// none of these strings, so every case is red without the component.

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

function payload(overrides: Record<string, unknown> = {}) {
  return {
    source_status: 'ok',
    postgres: {
      last_full: {
        ts: '2026-08-24T02:31:10Z',
        size_bytes: 412_316_860_416,
        repo: '1',
      },
      last_diff: { ts: '2026-08-29T02:04:41Z', size_bytes: null },
      wal_archive_max_age_seconds: 74,
      repos: [
        {
          repo: '1',
          kind: 'local',
          last_backup_ts: '2026-08-29T02:00:03Z',
          retention: null,
        },
        {
          repo: '2',
          kind: 'offsite',
          last_backup_ts: '2026-08-17T02:00:01Z',
          retention: null,
        },
      ],
    },
    restore_drill: {
      last_run_ts: '2026-08-02T04:00:12Z',
      last_success_ts: '2026-08-02T04:00:12Z',
      result: 'pass',
      failed_checks: 0,
      restored_backup_ts: null,
      duration_s: null,
    },
    clickhouse: {
      schema_snapshot_last_ts: '2026-08-29T03:40:05Z',
      schema_snapshot_offsite_last_ts: null,
      zfs_snapshot_latest_ts: null,
      replica_lag_s: null,
    },
    freshness: {
      full: verdict('ok', 5 * 86_400, SLO.full_seconds),
      diff: verdict('ok', 10 * 3_600, SLO.diff_seconds),
      wal: verdict('ok', 74, SLO.wal_seconds),
      // repo2's newest backup is 12 d old — past the 8 d off-site SLO.
      offsite: verdict('stale', 12 * 86_400 + 36_000, SLO.offsite_seconds),
      drill: verdict('ok', 27 * 86_400, SLO.drill_seconds),
      snapshot: verdict('ok', 8 * 3_600, SLO.snapshot_seconds),
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

  it('renders each item with its API-judged age and colours the breached off-site copy red', async () => {
    mockBackups({ data: payload(), as_of: '2026-08-29T12:00:00Z' });
    renderPanel();

    await waitFor(() =>
      expect(screen.getByText('Last full backup')).toBeInTheDocument(),
    );
    // Roll-up: one item is past its SLO → the headline says so.
    expect(screen.getByText('SLO breached')).toBeInTheDocument();
    expect(screen.queryByText('all within SLO')).not.toBeInTheDocument();

    // The off-site row is RED with its real age, not a green zero.
    const offsiteAge = screen.getByText('12d ago');
    expect(offsiteAge).toHaveClass('text-bad-700');
    expect(screen.getByText('beyond SLO')).toBeInTheDocument();
    expect(
      screen.getByText('newest backup 2026-08-17 02:00 UTC'),
    ).toBeInTheDocument();

    // Fresh rows show their ages from the API's age_seconds (not a
    // client clock) and the SLO they were judged against.
    expect(screen.getByText('5d ago')).toBeInTheDocument();
    // full + off-site share the 8 d SLO.
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
    mockBackups({
      data: payload({
        source_status: 'ok',
        postgres: {
          last_full: null,
          last_diff: null,
          wal_archive_max_age_seconds: null,
          repos: [],
        },
        restore_drill: {
          last_run_ts: null,
          last_success_ts: null,
          result: 'unknown',
          failed_checks: null,
          restored_backup_ts: null,
          duration_s: null,
        },
        clickhouse: {
          schema_snapshot_last_ts: null,
          schema_snapshot_offsite_last_ts: null,
          zfs_snapshot_latest_ts: null,
          replica_lag_s: null,
        },
        freshness: {
          full: verdict('unknown', null, SLO.full_seconds),
          diff: verdict('unknown', null, SLO.diff_seconds),
          wal: verdict('unknown', null, SLO.wal_seconds),
          offsite: verdict('unknown', null, SLO.offsite_seconds),
          drill: verdict('unknown', null, SLO.drill_seconds),
          snapshot: verdict('unknown', null, SLO.snapshot_seconds),
          overall: 'unknown',
        },
      }),
      as_of: '2026-08-29T12:00:00Z',
    });
    renderPanel();

    await waitFor(() =>
      expect(screen.getByText('Last full backup')).toBeInTheDocument(),
    );
    expect(screen.getAllByText('no data').length).toBe(6);
    expect(screen.getByText('partial data')).toBeInTheDocument();
    expect(screen.queryByText('within SLO')).not.toBeInTheDocument();
    expect(screen.queryByText(/0s ago/)).not.toBeInTheDocument();
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

  it('marks verdicts untrustworthy when the API could not read Prometheus', async () => {
    mockBackups({
      data: payload({ source_status: 'unknown' }),
      as_of: '2026-08-29T12:00:00Z',
    });
    renderPanel();

    await waitFor(() =>
      expect(
        screen.getByText('source unknown · verdicts not trustworthy'),
      ).toBeInTheDocument(),
    );
    expect(screen.queryByText('SLO breached')).not.toBeInTheDocument();
    expect(screen.queryByText('all within SLO')).not.toBeInTheDocument();
  });

  it('shows a failed drill as red with its failed-check count', async () => {
    mockBackups({
      data: payload({
        restore_drill: {
          last_run_ts: '2026-08-02T04:00:12Z',
          last_success_ts: '2026-07-05T04:00:12Z',
          result: 'fail',
          failed_checks: 2,
          restored_backup_ts: null,
          duration_s: null,
        },
      }),
      as_of: '2026-08-29T12:00:00Z',
    });
    renderPanel();

    await waitFor(() =>
      expect(screen.getByText('last run failed')).toBeInTheDocument(),
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
    expect(screen.queryByText('within SLO')).not.toBeInTheDocument();
  });
});
