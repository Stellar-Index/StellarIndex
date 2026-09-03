'use client';

import Link from 'next/link';

import { useCoverage, type CoverageVerdict, type RecognitionAxis } from '@/api/hooks';

/**
 * CoveragePanel — the decoder-coverage panel on /diagnostics.
 *
 * "Decoder coverage" IS the ADR-0033 completeness verdict: for each
 * indexed source, the audit proves (1) the raw-lake substrate is
 * contiguous, (2) every recognisable event was recognised, and
 * (3) the served-tier projection reconciles with the lake. This
 * panel renders `/v1/coverage` — the same rows the status page's
 * "N/N complete" headline is computed from — one row per source
 * with the three claims broken out so a red verdict says WHICH
 * claim failed.
 *
 * ADR-0034 two-axis verdict: `complete` (served tier — substrate +
 * recognition + projection, retention-scoped, since Postgres only
 * holds the recent working set) and `lake_complete` (archive tier —
 * substrate + recognition only, proven genesis-to-tip in the
 * certified ClickHouse lake, independent of served-tier retention).
 * A source can be `lake_complete: true, complete: false` — the full
 * history is proven, the served tier just hasn't reconciled it all.
 *
 * The ADR-0033 RECOGNITION axis is rendered separately, below the
 * table, by {@link RecognitionAxisCard}. It is a system-wide census —
 * event shapes on contracts no indexed source owns — not a source, and
 * it used to arrive as a 21st row here that read permanently
 * incomplete by construction, making the headline "20/21" unfixable.
 * It is a discovery backlog, not missing data, so it gets its own card
 * with its own numbers rather than a red row in a completeness table.
 */
export function CoveragePanel() {
  const { data, isLoading, error } = useCoverage();

  if (isLoading) {
    return (
      <section className="rounded-md border border-line bg-surface p-4 text-sm text-ink-muted">
        Loading coverage verdicts…
      </section>
    );
  }
  if (error || !data) {
    return (
      <section className="rounded-md border border-line bg-surface p-4 text-sm text-ink-muted">
        Coverage verdicts unavailable — the deployment may not have a
        completeness reader wired. See{' '}
        <code className="rounded-sm bg-surface-subtle px-1 font-mono text-[13px]">
          /v1/coverage
        </code>
        .
      </section>
    );
  }

  const allComplete = data.complete_sources === data.total_sources;
  const allLakeComplete = data.lake_complete_sources === data.total_sources;

  return (
    <section className="rounded-lg border border-line bg-surface p-4">
      <header className="mb-1 flex flex-wrap items-baseline justify-between gap-2">
        <div className="flex flex-wrap items-baseline gap-3">
          <h3 className="text-sm font-semibold uppercase tracking-wider text-ink-body">
            Completeness verdicts
          </h3>
          <span
            className={`rounded-sm px-2 py-0.5 font-mono text-xs tabular-nums ${
              allComplete ? 'bg-up-subtle text-up' : 'bg-down-subtle text-down'
            }`}
            title="Served tier: substrate + recognition + projection verified, scoped to Postgres's retention window."
          >
            {data.complete_sources}/{data.total_sources} served tier
          </span>
          <span
            className={`rounded-sm px-2 py-0.5 font-mono text-xs tabular-nums ${
              allLakeComplete ? 'bg-up-subtle text-up' : 'bg-down-subtle text-down'
            }`}
            title="Archive (lake): the certified ClickHouse archive is contiguous and hash-chained from genesis to tip — independent of the served tier's retention window."
          >
            {data.lake_complete_sources}/{data.total_sources} archive (lake)
          </span>
        </div>
        <span className="text-xs text-ink-faint">
          ADR-0033/0034 · /v1/coverage ·{' '}
          <Link href="/status" className="text-brand-600 hover:underline">
            status →
          </Link>
        </span>
      </header>
      <p className="mb-3 text-xs text-ink-faint">
        <strong className="font-medium text-ink-muted">Served tier</strong> = verified within
        Postgres&apos;s retention window (what the API queries).{' '}
        <strong className="font-medium text-ink-muted">Archive (lake)</strong> = the certified
        ClickHouse archive, proven genesis-to-tip regardless of retention.
      </p>
      <div className="-mx-4 overflow-x-auto">
        <table className="min-w-full divide-y divide-line text-sm">
          <thead>
            <tr className="text-left text-[10px] uppercase tracking-wider text-ink-muted">
              <th className="px-4 py-2 font-medium">Source</th>
              <th
                className="px-4 py-2 font-medium"
                title="Served tier: substrate + recognition + projection, retention-scoped."
              >
                Served
              </th>
              <th
                className="px-4 py-2 font-medium"
                title="Archive (lake): substrate + recognition, genesis-to-tip."
              >
                Lake
              </th>
              <th className="px-4 py-2 font-medium">Claims</th>
              <th className="px-4 py-2 text-right font-medium">Watermark</th>
              <th className="px-4 py-2 text-right font-medium">Coverage</th>
              <th className="px-4 py-2 text-right font-medium">Computed</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-line-subtle">
            {data.sources.map((v) => (
              <VerdictRow key={v.source} v={v} />
            ))}
          </tbody>
        </table>
      </div>

      <RecognitionAxisCard rec={data.recognition} />

      {data.not_applicable_sources && data.not_applicable_sources.length > 0 && (
        <p className="mt-3 text-xs text-ink-faint">
          <span className="text-ink-body">
            Not applicable on {data.network}:
          </span>{' '}
          {data.not_applicable_sources.map((na) => na.source).join(', ')}. These
          protocols are anchored to pubnet contract identities (ADR-0035), so
          they do not exist on this network — they are excluded from the totals
          above rather than counted incomplete.
        </p>
      )}
    </section>
  );
}

/**
 * RecognitionAxisCard — the ADR-0033 recognition census.
 *
 * Deliberately NOT a row in the verdict table and deliberately not
 * hidden: it is excluded from the "N/N" headline above because it is
 * an audit axis rather than a source, so it has to be MORE visible
 * here, not less, or the headline change is just flattery. It states
 * the numbers and, in plain language, what it does and does not mean.
 */
function RecognitionAxisCard({ rec }: { rec: RecognitionAxis }) {
  if (!rec) {
    return (
      <p className="mt-3 rounded-md border border-line bg-surface-subtle p-3 text-xs text-ink-faint">
        <span className="text-ink-body">Recognition axis:</span> no census
        published yet — the completeness audit has not produced one on this
        deployment. This is not a clean result; it is an absent one.
      </p>
    );
  }
  const shapes = rec.unrecognized_shapes;
  const contracts = rec.unrecognized_contracts;
  return (
    <div className="mt-3 rounded-md border border-line bg-surface-subtle p-3">
      <div className="mb-1 flex flex-wrap items-baseline gap-3">
        <h4 className="text-xs font-semibold uppercase tracking-wider text-ink-body">
          Recognition axis — unclaimed contracts
        </h4>
        <span
          className={`rounded-sm px-2 py-0.5 font-mono text-xs tabular-nums ${
            rec.all_shapes_recognized
              ? 'bg-up-subtle text-up'
              : 'bg-surface text-ink-muted'
          }`}
          title="A system-wide census, not a source verdict: it is excluded from the N/N headline above."
        >
          {rec.all_shapes_recognized
            ? 'every shape claimed'
            : 'discovery backlog'}
        </span>
      </div>
      <p className="text-xs text-ink-muted">
        {shapes != null ? (
          <>
            <strong className="font-mono tabular-nums text-ink-body">
              {shapes.toLocaleString('en-US')}
            </strong>{' '}
            distinct event shape{shapes === 1 ? '' : 's'}
            {contracts != null && (
              <>
                {' '}on{' '}
                <strong className="font-mono tabular-nums text-ink-body">
                  {contracts.toLocaleString('en-US')}
                </strong>{' '}
                contract{contracts === 1 ? '' : 's'}
              </>
            )}{' '}
            that no indexed source owns
            {rec.earliest_ledger != null && rec.earliest_ledger > 0 && (
              <>
                , first seen at ledger{' '}
                <span className="font-mono tabular-nums">
                  #{rec.earliest_ledger.toLocaleString('en-US')}
                </span>
              </>
            )}
            .
          </>
        ) : (
          // Counts omitted rather than zeroed when the audit's detail
          // does not parse — never render an unverified "0".
          <>Census counts unavailable for this snapshot; the audit&apos;s own
          detail is below.</>
        )}
      </p>
      <p className="mt-1 text-xs text-ink-faint">
        These are Soroban protocols Stellar Index has not integrated —{' '}
        <strong className="font-medium text-ink-muted">
          a discovery backlog, not missing data
        </strong>
        . No source in the table above is dropping events because of it, and
        this number can never reach zero while contracts we do not index keep
        being deployed, which is why it is not counted in the headline. A
        source silently dropping its <em>own</em> events is the opposite case:
        that shows as a red <code className="font-mono">recognition</code>{' '}
        claim on that source&apos;s row, and it does fail the headline.
      </p>
      {rec.detail && (
        <p className="mt-1 font-mono text-[11px] leading-relaxed text-ink-faint">
          {rec.detail}
        </p>
      )}
    </div>
  );
}

function VerdictRow({ v }: { v: CoverageVerdict }) {
  return (
    <tr className="hover:bg-surface-muted" title={v.detail ?? undefined}>
      <td className="px-4 py-2 font-mono text-xs">
        <Link
          href={`/sources/${encodeURIComponent(v.source)}`}
          className="hover:text-brand-600"
        >
          {v.source}
        </Link>
      </td>
      <td className="px-4 py-2">
        <span
          className={`inline-flex items-center rounded-sm px-2 py-0.5 font-mono text-xs ${
            v.complete ? 'bg-up-subtle text-up' : 'bg-down-subtle text-down'
          }`}
          title="Served tier: substrate + recognition + projection, retention-scoped."
        >
          {v.complete ? 'complete' : 'incomplete'}
        </span>
      </td>
      <td className="px-4 py-2">
        <span
          className={`inline-flex items-center rounded-sm px-2 py-0.5 font-mono text-xs ${
            v.lake_complete ? 'bg-up-subtle text-up' : 'bg-down-subtle text-down'
          }`}
          title="Archive (lake): substrate + recognition, genesis-to-tip — independent of served-tier retention."
        >
          {v.lake_complete ? 'genesis-complete' : 'partial'}
        </span>
      </td>
      <td className="px-4 py-2">
        <span className="inline-flex gap-1.5">
          <Claim label="substrate" ok={v.substrate_ok} />
          <Claim label="recognition" ok={v.recognition_ok} />
          <Claim label="projection" ok={v.projection_ok} />
        </span>
      </td>
      <td className="px-4 py-2 text-right font-mono text-xs tabular-nums">
        #{v.watermark_ledger.toLocaleString('en-US')}
      </td>
      <td className="px-4 py-2 text-right font-mono text-xs tabular-nums">
        {(v.coverage_pct * 100).toFixed(v.coverage_pct >= 1 ? 0 : 2)}%
      </td>
      <td className="px-4 py-2 text-right font-mono text-xs text-ink-muted">
        {v.computed_at.replace('T', ' ').slice(0, 16)} UTC
      </td>
    </tr>
  );
}

function Claim({ label, ok }: { label: string; ok: boolean }) {
  return (
    <span
      className={`rounded-sm px-1.5 py-0.5 text-[10px] uppercase tracking-wider ${
        ok ? 'bg-surface-subtle text-ink-muted' : 'bg-down-subtle text-down'
      }`}
      title={`${label} ${ok ? 'verified' : 'FAILED'}`}
    >
      {label}
    </span>
  );
}
