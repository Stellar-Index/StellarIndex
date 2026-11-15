'use client';

import Link from 'next/link';

import type { CoverageVerdict } from '@/api/hooks';

export const COVERAGE_AXES = [
  { key: 'substrate', label: 'Substrate' },
  { key: 'recognition', label: 'Recognition' },
  { key: 'projection', label: 'Projection' },
  { key: 'served', label: 'Served tier' },
  { key: 'lake', label: 'Archive lake' },
] as const;

export type CoverageAxis = (typeof COVERAGE_AXES)[number]['key'];

/** coverageCells — one verdict as five pass/fail cells, in COVERAGE_AXES order. */
export function coverageCells(
  v: CoverageVerdict,
): { axis: CoverageAxis; ok: boolean }[] {
  const ok: Record<CoverageAxis, boolean> = {
    substrate: v.substrate_ok,
    recognition: v.recognition_ok,
    projection: v.projection_ok,
    served: v.complete,
    lake: v.lake_complete,
  };
  return COVERAGE_AXES.map((a) => ({ axis: a.key, ok: ok[a.key] === true }));
}

/**
 * CoverageGrid — every source's verdict as a row of cells, so a red claim
 * is found at a glance before reading the table. A missing flag reads as
 * failed, never as passed.
 */
export function CoverageGrid({
  sources,
  staleSources,
}: {
  sources: readonly CoverageVerdict[];
  /** Sources whose verdict describes an older tip: a pass there is not a current claim. */
  staleSources?: ReadonlySet<string>;
}) {
  if (sources.length === 0) return null;
  const anyStale = sources.some((v) => staleSources?.has(v.source));
  return (
    <div className="mb-4 space-y-2" data-testid="coverage-grid">
      <div
        role="table"
        aria-label="Completeness claims per source"
        className="grid gap-x-6 gap-y-1 sm:grid-cols-2 lg:grid-cols-3"
      >
        {sources.map((v) => (
          <div
            key={v.source}
            role="row"
            className="flex items-center justify-between gap-3"
          >
            <Link
              role="rowheader"
              href={`/sources/${encodeURIComponent(v.source)}`}
              className="text-ink-body hover:text-brand-600 truncate font-mono text-xs"
            >
              {v.source}
            </Link>
            <span className="flex shrink-0 gap-0.5">
              {coverageCells(v).map((c) => {
                const label = COVERAGE_AXES.find(
                  (a) => a.key === c.axis,
                )!.label;
                const stale = c.ok && staleSources?.has(v.source) === true;
                const state = stale
                  ? 'verified at an older tip (stale)'
                  : c.ok
                    ? 'verified'
                    : 'failed';
                return (
                  <span
                    key={c.axis}
                    role="cell"
                    aria-label={`${label} ${state}`}
                    title={`${v.source}: ${label} ${state}`}
                    className={`inline-block h-3.5 w-3.5 rounded-xs ${
                      stale ? 'bg-warn-500' : c.ok ? 'bg-up' : 'bg-down'
                    } ${c.axis === 'served' ? 'ml-1' : ''}`}
                  />
                );
              })}
            </span>
          </div>
        ))}
      </div>
      <p className="text-ink-faint text-[11px]">
        Cells: {COVERAGE_AXES.map((a) => a.label.toLowerCase()).join(', ')}.
        Green verified, red failed
        {anyStale ? ', amber verified at an older tip' : ''}.
      </p>
    </div>
  );
}
