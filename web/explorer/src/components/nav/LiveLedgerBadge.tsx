'use client';

// LiveLedgerBadge — the sidebar's heartbeat (live-tick program RT-2).
// Subscribes to /v1/ledger/stream via the shared multiplexer and shows
// the latest closed ledger with a pulse; every tick nudges the number.
// Renders nothing while the stream is unavailable or silent (WB-04:
// a quiet stream must not claim "live"), so older API binaries or
// blocked SSE degrade to the exact pre-badge sidebar.

import Link from 'next/link';

import {
  isFrameStale,
  LEDGER_LIVE_STALE_MS,
  useLedgerStream,
  useLiveClock,
} from '@/lib/live/hooks';

export function LiveLedgerBadge({ onNavigate }: { onNavigate?: () => void }) {
  const frame = useLedgerStream();
  // Slow clock so a wedged stream drops the badge without needing a
  // new frame to trigger a render (WB-04).
  const clock = useLiveClock();

  if (!frame || isFrameStale(clock, frame.receivedAt, LEDGER_LIVE_STALE_MS)) return null;

  const seq = frame.data.latest_ledger;
  return (
    <div className="px-3 pb-2">
      <Link
        href={`/ledgers/${seq}`}
        onClick={onNavigate}
        className="flex items-center gap-2 rounded-lg border border-line bg-surface px-2.5 py-1.5 text-xs hover:bg-surface-subtle"
        aria-label={`Latest ledger ${seq}`}
      >
        <span className="relative flex h-2 w-2 shrink-0" aria-hidden>
          <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-up opacity-60" />
          <span className="relative inline-flex h-2 w-2 rounded-full bg-up" />
        </span>
        <span className="text-ink-muted">Ledger</span>
        <span key={seq} className="font-mono tabular-nums text-ink live-tick">
          {seq.toLocaleString('en-US')}
        </span>
      </Link>
    </div>
  );
}
