'use client';

import { useState, useMemo } from 'react';
import { ArrowLeftRight } from 'lucide-react';

import { useConvertRate } from './ConvertLive';

/**
 * ConvertPair — interactive client-side converter for the
 * `/convert/[from]/[to]` static page. The server-rendered page
 * shows the headline rate and snippet ladder; this component lets
 * the visitor type a custom amount.
 *
 * The live rate comes from the shared `useConvertRate` hook so this
 * widget, the header, and the ladder all read ONE deduped
 * `/v1/price/batch` query (refreshed every 60s). SSR-rendered initial
 * value comes from `initialRate` so the first paint is correct without
 * a client roundtrip.
 */
export function ConvertPair({
  from,
  to,
  initialRate,
  initialInverse,
}: {
  from: string;
  to: string;
  initialRate: number | null;
  initialInverse: number | null;
}) {
  const [amount, setAmount] = useState('1');
  const [direction, setDirection] = useState<'forward' | 'reverse'>('forward');

  // Live-refresh the rate so the converter doesn't go stale. Shared
  // with the header + ladder so all three read ONE deduped query.
  const { rate, inverse, updatedAt } = useConvertRate({
    from,
    to,
    initialRate,
    initialInverse,
  });

  const numeric = Number(amount);
  const result = useMemo(() => {
    if (!Number.isFinite(numeric) || rate == null || inverse == null) return null;
    return direction === 'forward' ? numeric * rate : numeric * inverse;
  }, [numeric, rate, inverse, direction]);

  const fromLabel = direction === 'forward' ? from : to;
  const toLabel = direction === 'forward' ? to : from;

  return (
    <section className="rounded-xl border border-line bg-surface p-5 shadow-sm">
      <h2 className="mb-4 text-lg font-semibold tracking-tight">
        Convert {fromLabel} → {toLabel}
      </h2>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-[1fr_auto_1fr]">
        <label className="space-y-1">
          <span className="text-xs uppercase tracking-wider text-ink-muted">From</span>
          <div className="flex items-center gap-2 rounded-md border border-line bg-surface p-2">
            <input
              type="number"
              value={amount}
              onChange={(e) => setAmount(e.target.value)}
              min="0"
              step="any"
              inputMode="decimal"
              className="w-full bg-transparent text-2xl font-mono tabular-nums focus:outline-hidden"
              aria-label={`Amount in ${fromLabel}`}
            />
            <span className="rounded-sm bg-surface-subtle px-1.5 py-0.5 font-mono text-xs uppercase tracking-wider text-ink-body">
              {fromLabel}
            </span>
          </div>
        </label>

        <button
          type="button"
          onClick={() => setDirection((d) => (d === 'forward' ? 'reverse' : 'forward'))}
          className="self-end rounded-md border border-line bg-surface p-2 text-ink-muted hover:border-brand-500 hover:text-brand-600"
          aria-label="Swap direction"
          title="Swap direction"
        >
          <ArrowLeftRight className="h-4 w-4" />
        </button>

        <label className="space-y-1">
          <span className="text-xs uppercase tracking-wider text-ink-muted">To</span>
          <div className="flex items-center gap-2 rounded-md border border-line bg-surface p-2">
            <span className="w-full text-2xl font-mono tabular-nums text-ink">
              {result != null ? formatRate(result) : '—'}
            </span>
            <span className="rounded-sm bg-surface-subtle px-1.5 py-0.5 font-mono text-xs uppercase tracking-wider text-ink-body">
              {toLabel}
            </span>
          </div>
        </label>
      </div>
      <p className="mt-3 text-xs text-ink-muted">
        {rate != null && inverse != null ? (
          <>
            1 {fromLabel} ={' '}
            <span className="font-mono tabular-nums">
              {formatRate(direction === 'forward' ? rate : inverse)}
            </span>{' '}
            {toLabel}
            {updatedAt > 0 && (
              <>
                <span className="mx-1.5">·</span>
                Updated {formatRelativeTime(updatedAt)}
              </>
            )}
          </>
        ) : (
          'Rate currently unavailable.'
        )}
      </p>
    </section>
  );
}

function formatRate(n: number): string {
  if (!Number.isFinite(n)) return '—';
  if (Math.abs(n) >= 1000) return n.toLocaleString('en-US', { maximumFractionDigits: 2 });
  if (Math.abs(n) >= 1) return n.toFixed(4);
  if (Math.abs(n) >= 0.01) return n.toFixed(6);
  return n.toFixed(8);
}

function formatRelativeTime(ms: number): string {
  const diff = Date.now() - ms;
  if (diff < 5_000) return 'just now';
  if (diff < 60_000) return `${Math.floor(diff / 1000)}s ago`;
  if (diff < 3600_000) return `${Math.floor(diff / 60_000)}m ago`;
  return new Date(ms).toLocaleTimeString('en-US');
}
