'use client';

// NetworkSwitcher — the nav's network context + cross-explorer hop. Sits by
// the live-ledger odometer: shows THIS explorer's network (e.g. "mainnet")
// with a live dot, and a chevron that opens a list of the sibling networks
// with their own latest ledger, each linking to its explorer. The sibling
// tips are fetched lazily (only when the menu opens) from each network's
// grey api.* origin, and degrade to a dash if that origin is unreachable
// (cross-origin blocked / network down) so the hop link always works.

import { ArrowUpRight, Check, ChevronDown } from 'lucide-react';
import { useCallback, useEffect, useRef, useState } from 'react';

import { RollingNumber } from '@/components/primitives/RollingNumber';
import {
  isFrameStale,
  LEDGER_LIVE_STALE_MS,
  useLedgerStream,
  useLiveClock,
} from '@/lib/live/hooks';
import { CURRENT_NETWORK, NETWORKS, type NetworkInfo } from '@/lib/networks';
import { useDialog } from '@/lib/useDialog';

// A sibling's tip: a ledger number, `null` if its origin was unreachable
// (cross-origin blocked / offline), or absent from the map = not fetched yet
// (loading). We never setState synchronously in the effect — only in the
// async resolution below — so the map has no explicit "loading" flag.
type TipMap = Record<string, number | null>;

async function fetchTip(apiBaseUrl: string, signal: AbortSignal): Promise<number | null> {
  try {
    const res = await fetch(`${apiBaseUrl}/v1/ledger/tip`, {
      signal,
      headers: { Accept: 'application/json' },
    });
    if (!res.ok) return null;
    const body = (await res.json()) as { data?: { latest_ledger?: unknown } };
    const seq = body?.data?.latest_ledger;
    return typeof seq === 'number' ? seq : null;
  } catch {
    return null; // aborted, CORS-blocked, or offline — caller shows a dash
  }
}

// Dot tones: 'live' = the current network's fresh SSE stream (pulsing green);
// 'up' = a sibling whose one-shot tip probe succeeded (static green); 'muted'
// = stale / loading / unreachable / not-yet-deployed (grey, no pulse).
type DotTone = 'live' | 'up' | 'muted';

function LiveDot({ tone }: { tone: DotTone }) {
  if (tone === 'live') {
    return (
      <span className="relative flex h-1.5 w-1.5 shrink-0" aria-hidden>
        <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-up opacity-60" />
        <span className="relative inline-flex h-1.5 w-1.5 rounded-full bg-up" />
      </span>
    );
  }
  return (
    <span
      className={`h-1.5 w-1.5 shrink-0 rounded-full ${tone === 'up' ? 'bg-up' : 'bg-ink-faint'}`}
      aria-hidden
    />
  );
}

export function NetworkSwitcher({ onNavigate }: { onNavigate?: () => void }) {
  const [open, setOpen] = useState(false);
  const wrapRef = useRef<HTMLDivElement>(null);
  const close = useCallback(() => setOpen(false), []);
  const panelRef = useDialog<HTMLDivElement>(open, close);
  const [tips, setTips] = useState<TipMap>({});

  // Current network's live tip drives the trigger dot + its own dropdown row.
  const frame = useLedgerStream();
  const clock = useLiveClock();
  const currentLive =
    !!frame && !isFrameStale(clock, frame.receivedAt, LEDGER_LIVE_STALE_MS);
  const currentSeq = frame?.data.latest_ledger ?? null;

  // Close on outside click (matches the account-menu disclosure).
  useEffect(() => {
    if (!open) return;
    function onDoc(e: MouseEvent) {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) setOpen(false);
    }
    document.addEventListener('mousedown', onDoc);
    return () => document.removeEventListener('mousedown', onDoc);
  }, [open]);

  // Lazily probe the sibling networks' tips when the menu opens. setState
  // happens only in the async .then (never synchronously in the effect body),
  // so a first open shows '···' until each fetch resolves.
  useEffect(() => {
    if (!open) return;
    const ac = new AbortController();
    for (const n of NETWORKS) {
      if (n.id === CURRENT_NETWORK.id || !n.live) continue;
      void fetchTip(n.apiBaseUrl, ac.signal).then((seq) =>
        setTips((t) => ({ ...t, [n.id]: seq })),
      );
    }
    return () => ac.abort();
  }, [open]);

  return (
    <div ref={wrapRef} className="relative">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
        aria-controls="network-switcher-menu"
        aria-label={`Network: ${CURRENT_NETWORK.label}${
          currentSeq != null ? `, ledger ${currentSeq}` : ''
        }. Switch explorer`}
        className="flex flex-col items-start gap-0.5 rounded-md px-1.5 py-1 text-left hover:bg-surface-subtle"
      >
        {/* The whole odometer IS the switcher trigger: capitalized network name
            + chevron on top, this network's live ledger (single live dot +
            rolling number) beneath. Not a link — clicking anywhere on it opens
            the network dropdown. */}
        <span className="flex items-center gap-1 text-[11px] text-ink-muted">
          {CURRENT_NETWORK.label}
          <ChevronDown
            className={`h-3 w-3 text-ink-faint transition-transform ${open ? 'rotate-180' : ''}`}
            aria-hidden
          />
        </span>
        <span className="flex items-center gap-1.5 text-[11px]">
          <LiveDot tone={currentLive ? 'live' : 'muted'} />
          {currentSeq != null ? (
            <RollingNumber value={currentSeq} className="font-mono text-ink" />
          ) : (
            <span className="font-mono text-ink-faint">—</span>
          )}
        </span>
      </button>

      {open && (
        // Disclosure, not an APG menu (matches AccountMenu): the trigger's
        // aria-expanded/-controls describe it; rows are plain links, Tab-
        // reachable, Escape-closable (useDialog).
        <div
          id="network-switcher-menu"
          ref={panelRef}
          tabIndex={-1}
          className="absolute left-0 top-full z-50 mt-1 w-60 rounded-lg border border-line bg-surface p-1.5 shadow-elevated outline-hidden"
        >
          <p className="px-2 py-1 text-[10px] font-medium uppercase tracking-wide text-ink-faint">
            Networks
          </p>
          {NETWORKS.map((n) => (
            <NetworkRow
              key={n.id}
              network={n}
              current={n.id === CURRENT_NETWORK.id}
              currentLive={currentLive}
              currentSeq={currentSeq}
              tip={tips[n.id]}
              onNavigate={() => {
                setOpen(false);
                onNavigate?.();
              }}
            />
          ))}
        </div>
      )}
    </div>
  );
}

// tip: a number renders (odometer); undefined = still loading ('···');
// null = the origin was unreachable ('—').
function TipNumber({ tip }: { tip: number | null | undefined }) {
  if (typeof tip === 'number') return <RollingNumber value={tip} className="font-mono text-ink-muted" />;
  return <span className="font-mono text-ink-faint">{tip === undefined ? '···' : '—'}</span>;
}

function NetworkRow({
  network,
  current,
  currentLive,
  currentSeq,
  tip,
  onNavigate,
}: {
  network: NetworkInfo;
  current: boolean;
  currentLive: boolean;
  currentSeq: number | null;
  tip?: number | null;
  onNavigate: () => void;
}) {
  const rowInner = (
    <>
      <LiveDot tone={current ? (currentLive ? 'live' : 'muted') : typeof tip === 'number' ? 'up' : 'muted'} />
      <span className="flex-1 truncate text-ink">{network.label}</span>
      {current ? (
        <>
          <TipNumber tip={currentSeq} />
          <Check className="h-3.5 w-3.5 shrink-0 text-brand-600" aria-label="Current" />
        </>
      ) : network.live ? (
        <>
          <TipNumber tip={tip} />
          <ArrowUpRight className="h-3.5 w-3.5 shrink-0 text-ink-faint" aria-hidden />
        </>
      ) : (
        <span className="text-[10px] uppercase tracking-wide text-ink-faint">Soon</span>
      )}
    </>
  );

  const rowClass =
    'flex items-center gap-2 rounded-md px-2 py-1.5 text-xs';

  // Current network: not a link (you're here). Not-yet-live: disabled.
  if (current || !network.live) {
    return (
      <div
        className={`${rowClass} ${current ? 'bg-surface-subtle' : 'opacity-60'}`}
        aria-current={current ? 'page' : undefined}
      >
        {rowInner}
      </div>
    );
  }

  // Sibling explorer lives on another origin → full-page nav, not client-side.
  return (
    <a href={network.explorerUrl} onClick={onNavigate} className={`${rowClass} hover:bg-surface-subtle`}>
      {rowInner}
    </a>
  );
}
