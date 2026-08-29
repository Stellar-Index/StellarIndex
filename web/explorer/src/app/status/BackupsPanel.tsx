'use client';

import { Badge, Card, type BadgeTone } from '@/components/ui';
import { useBackupsDiagnostics, type BackupsDiagnostics } from '@/api/hooks';
import { formatDurationShort } from '@/lib/format';

// BackupsPanel — the public status page's "Backups" block: last
// pgBackRest full / diff / WAL-archive age, the off-site (S3) copy's
// age, the monthly restore drill (pass/fail + date), and the ClickHouse
// schema-snapshot age, each judged against the SLO the API echoes.
//
// Honest-staleness contract (PR #273's conventions): a row whose source
// is absent renders grey "no data" — never a green zero; a row past its
// SLO renders red with its real age; the whole panel carries a
// "source unknown / degraded" marker when the API says its Prometheus
// reads failed. Every age is the API's own `age_seconds` (judged at the
// server's `as_of`), not a client-side Date.now() — so what the panel
// colours is exactly what the verdict was judged on.
//
// Lives in its own file, mounted from StatusPageClient with a one-line
// hook, so it composes with the status-page refactors in flight.

type Verdict = BackupsDiagnostics['freshness']['full'];

// Tone per verdict: ok → green, stale → red (SLO breached, not merely
// "warn": a backup past its SLO is a definite finding), unknown → grey.
function verdictTone(status: Verdict['status']): BadgeTone {
  switch (status) {
    case 'ok':
      return 'ok';
    case 'stale':
      return 'bad';
    default:
      return 'neutral';
  }
}

function verdictLabel(v: Verdict): string {
  switch (v.status) {
    case 'ok':
      return 'within SLO';
    case 'stale':
      return 'beyond SLO';
    default:
      // The API pairs an "unknown" verdict with a non-null age in
      // exactly one case: a stamp from the FUTURE of its clock (skew,
      // or a corrupt backup label — issue #311). Name that cause; it
      // is a different problem from an absent series, and the reader
      // must not read the grey row as "nothing is exported here".
      return isFutureDated(v) ? 'stamp from the future' : 'no data';
  }
}

// isFutureDated: the API's negative-age signal. A future-dated stamp
// bounds nothing about the item's real age, so the API refuses to
// judge it — see freshnessVerdict in internal/api/v1.
function isFutureDated(v: Verdict): boolean {
  return (
    v.age_seconds !== null && v.age_seconds !== undefined && v.age_seconds < 0
  );
}

// ageText renders a verdict's age as "5d ago" from the API's
// age_seconds; absent — or future-dated, where the number is a skew
// measurement and not an age at all — → the em-dash the rest of the
// page uses for unmeasured values.
function ageText(v: Verdict): string {
  if (v.age_seconds === null || v.age_seconds === undefined) return '—';
  if (isFutureDated(v)) return '—';
  return `${formatDurationShort(v.age_seconds)} ago`;
}

// dateText renders an ISO timestamp as a compact UTC date-time; null →
// "never" (the API's null means the series is absent, which for a
// backup stamp reads as "never recorded").
function dateText(iso: string | null | undefined): string {
  if (!iso) return 'never';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return 'never';
  return d.toISOString().slice(0, 16).replace('T', ' ') + ' UTC';
}

// sinceAsOf dates an ISO stamp against the server's as_of (not
// Date.now()): the age the API judged is the age the panel shows.
//
// A stamp AHEAD of as_of is the same future-dated artefact the API
// refuses to judge (#311) — clock skew or a corrupt pgBackRest label.
// It is NOT a zero-second-old copy, so it is named rather than clamped
// up into "0s ago", which is how this row previously painted an
// arbitrarily stale repository as the freshest thing on the page.
function sinceAsOf(iso: string | null | undefined, asOf?: string): string {
  if (!iso || !asOf) return '—';
  const ms = new Date(asOf).getTime() - new Date(iso).getTime();
  if (!Number.isFinite(ms)) return '—';
  if (ms < 0) return 'dated in the future';
  return `${formatDurationShort(ms / 1000)} ago`;
}

function sloText(seconds: number): string {
  return `SLO ≤ ${formatDurationShort(seconds)}`;
}

// bytesText renders a byte count in binary units (the unit pgBackRest
// and ZFS report in), one decimal, so a 384 GiB full reads as such and
// never as "412.32B" (compact-billions + "B" for bytes).
function bytesText(n: number | null | undefined): string {
  if (n === null || n === undefined || !Number.isFinite(n)) return '—';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  return `${i === 0 ? v : v.toFixed(1)} ${units[i]}`;
}

export default function BackupsPanel() {
  const { data: env, error, isPending } = useBackupsDiagnostics();
  const doc = env?.data;

  return (
    <section aria-labelledby="backups-heading">
      <div className="mb-3 flex items-baseline justify-between gap-3">
        <h2
          id="backups-heading"
          className="text-ink-muted text-sm font-semibold tracking-wider uppercase"
        >
          Backups
          <span className="text-ink-faint ml-2 text-xs font-normal tracking-normal normal-case">
            · freshness vs. SLO, from the host&apos;s own exporters
          </span>
        </h2>
        {doc && <OverallBadge doc={doc} />}
      </div>

      {error && !doc && (
        <Card className="border-line bg-surface-muted text-ink-muted px-4 py-3 text-sm">
          Backup freshness unavailable: {error.message}. No backup state is
          shown — this is an absence of data, not an all-clear.
        </Card>
      )}
      {isPending && !doc && !error && (
        <Card className="text-ink-faint px-4 py-6 text-center text-sm">
          Loading backup freshness…
        </Card>
      )}

      {doc && (
        <div className="grid gap-4 sm:grid-cols-2">
          <PostgresCard doc={doc} asOf={env?.as_of} />
          <DrillAndLakeCard doc={doc} asOf={env?.as_of} />
        </div>
      )}
    </section>
  );
}

// OverallBadge — the panel's roll-up. `source_status` outranks the
// freshness roll-up in the label: an "unknown" source means the
// verdicts below are blind, and the reader must know that first.
function OverallBadge({ doc }: { doc: BackupsDiagnostics }) {
  if (doc.source_status === 'unknown') {
    return (
      <Badge tone="neutral" dot>
        source unknown · verdicts not trustworthy
      </Badge>
    );
  }
  const tone = verdictTone(doc.freshness.overall);
  const label =
    doc.freshness.overall === 'ok'
      ? 'all within SLO'
      : doc.freshness.overall === 'stale'
        ? 'SLO breached'
        : 'partial data';
  return (
    <span className="flex items-center gap-2">
      {doc.source_status === 'degraded' && (
        <Badge tone="neutral">source degraded</Badge>
      )}
      <Badge tone={tone} dot>
        {label}
      </Badge>
    </span>
  );
}

function PostgresCard({
  doc,
  asOf,
}: {
  doc: BackupsDiagnostics;
  asOf?: string;
}) {
  const { postgres: pg, freshness: f, slo } = doc;
  const offsite = pg.repos.find((r) => r.kind === 'offsite');
  return (
    <div className="border-line bg-surface-muted rounded-lg border p-4">
      <h3 className="text-ink-faint mb-2 text-[11px] font-semibold tracking-wider uppercase">
        Postgres (pgBackRest)
      </h3>
      <dl className="space-y-2 text-sm">
        <VerdictRow
          label="Last full backup"
          verdict={f.full}
          detail={
            pg.last_full
              ? `${dateText(pg.last_full.ts)} · ${bytesText(pg.last_full.size_bytes)}${
                  pg.last_full.repo ? ` · repo ${pg.last_full.repo}` : ''
                }`
              : 'never recorded'
          }
          slo={slo.full_seconds}
        />
        <VerdictRow
          label="Last differential"
          verdict={f.diff}
          detail={pg.last_diff ? dateText(pg.last_diff.ts) : 'never recorded'}
          slo={slo.diff_seconds}
        />
        <VerdictRow
          label="WAL archive age"
          verdict={f.wal}
          detail={
            pg.wal_archive_max_age_seconds === null
              ? 'archiver age not exported on this host'
              : 'newest archived segment'
          }
          slo={slo.wal_seconds}
        />
        <VerdictRow
          label="Off-site copy (S3, repo 2)"
          verdict={f.offsite}
          detail={
            offsite
              ? `newest backup ${dateText(offsite.last_backup_ts)}`
              : 'no off-site repository reported'
          }
          slo={slo.offsite_seconds}
        />
        {pg.repos.length > 0 && (
          <div className="text-ink-faint pt-1 text-xs">
            Repositories:{' '}
            {pg.repos.map((r, i) => (
              <span key={r.repo}>
                {i > 0 && ' · '}
                repo {r.repo} ({r.kind}) {sinceAsOf(r.last_backup_ts, asOf)}
              </span>
            ))}
          </div>
        )}
      </dl>
    </div>
  );
}

function DrillAndLakeCard({
  doc,
  asOf,
}: {
  doc: BackupsDiagnostics;
  asOf?: string;
}) {
  const { restore_drill: drill, clickhouse: ch, freshness: f, slo } = doc;
  const drillDetail =
    drill.result === 'unknown'
      ? 'no drill recorded'
      : `last run ${dateText(drill.last_run_ts)} · ${
          drill.result === 'pass'
            ? 'passed'
            : `FAILED (${drill.failed_checks ?? '?'} check${
                drill.failed_checks === 1 ? '' : 's'
              })`
        }`;
  return (
    <div className="border-line bg-surface-muted rounded-lg border p-4">
      <h3 className="text-ink-faint mb-2 text-[11px] font-semibold tracking-wider uppercase">
        Restore drill &amp; ClickHouse
      </h3>
      <dl className="space-y-2 text-sm">
        <VerdictRow
          label="Last successful restore drill"
          verdict={f.drill}
          detail={drillDetail}
          slo={slo.drill_seconds}
          resultTone={
            drill.result === 'fail'
              ? 'bad'
              : drill.result === 'pass'
                ? 'ok'
                : 'neutral'
          }
          resultLabel={
            drill.result === 'fail'
              ? 'last run failed'
              : drill.result === 'pass'
                ? 'last run passed'
                : undefined
          }
        />
        <VerdictRow
          label="ClickHouse schema snapshot"
          verdict={f.snapshot}
          detail={
            ch.schema_snapshot_last_ts
              ? `${dateText(ch.schema_snapshot_last_ts)}${
                  ch.schema_snapshot_offsite_last_ts
                    ? ` · off-site ${sinceAsOf(ch.schema_snapshot_offsite_last_ts, asOf)}`
                    : ' · no off-site push recorded'
                }`
              : 'never recorded'
          }
          slo={slo.snapshot_seconds}
        />
        <div className="text-ink-faint pt-1 text-xs">
          ZFS snapshot:{' '}
          {ch.zfs_snapshot_latest_ts
            ? dateText(ch.zfs_snapshot_latest_ts)
            : 'no data'}{' '}
          · replica lag:{' '}
          {ch.replica_lag_s === null || ch.replica_lag_s === undefined
            ? 'no data'
            : `${formatDurationShort(ch.replica_lag_s)}`}
        </div>
      </dl>
    </div>
  );
}

// VerdictRow — one line: label, the verdict badge (green/red/grey),
// the age the verdict was judged on, the SLO, and a detail caption.
function VerdictRow({
  label,
  verdict,
  detail,
  slo,
  resultTone,
  resultLabel,
}: {
  label: string;
  verdict: Verdict;
  detail: string;
  slo: number;
  resultTone?: BadgeTone;
  resultLabel?: string;
}) {
  const tone = verdictTone(verdict.status);
  const ageClass =
    verdict.status === 'stale'
      ? 'text-bad-700'
      : verdict.status === 'ok'
        ? 'text-ink'
        : 'text-ink-faint';
  return (
    <div>
      <div className="flex items-baseline justify-between gap-3">
        <dt className="text-ink-muted text-xs">{label}</dt>
        <dd className="flex items-center gap-2">
          {resultLabel && resultTone && (
            <Badge tone={resultTone} className="text-[10px]">
              {resultLabel}
            </Badge>
          )}
          <Badge tone={tone} className="text-[10px]" dot>
            {verdictLabel(verdict)}
          </Badge>
          <span className={`tnum text-sm ${ageClass}`}>{ageText(verdict)}</span>
        </dd>
      </div>
      <div className="text-ink-faint mt-0.5 flex justify-between gap-3 text-[11px]">
        <span>{detail}</span>
        <span className="whitespace-nowrap">{sloText(slo)}</span>
      </div>
    </div>
  );
}
