'use client';

// Shared types + small UI primitives for the network-explorer pages
// (ledgers / ledger / tx / contract). ADR-0038 Phase D.
//
// The explorer entities (tx hash, ledger seq, contract id) are
// UNBOUNDED, so these pages are static query-param pages that read
// their param client-side via useSearchParams and fetch at runtime —
// they intentionally do NOT use [param] dynamic routes (which under
// output:'export' only pre-render generateStaticParams and 404 on
// unknown params).

import { Check, Copy } from 'lucide-react';
import { useEffect, useState } from 'react';

import type { components, paths } from '@/api/types';

// ---------------------------------------------------------------------------
// Wire shapes — every endpoint is wrapped as { data, as_of, flags }.
//
// Derived from the generated OpenAPI contract (src/api/types.ts,
// `make web-generate-api`) so spec drift fails the build instead of
// shipping as undefined in the UI. Fields the API serves but the spec
// doesn't declare yet are kept as `SPEC-GAP:` intersections.
// ---------------------------------------------------------------------------

type Schemas = components['schemas'];

// GetJSON extracts the application/json body of a GET 200 response for
// endpoints whose response shape is declared inline on the path.
type GetJSON<P extends keyof paths> = paths[P] extends {
  get: {
    responses: { 200: { content: { 'application/json': infer B } } };
  };
}
  ? B
  : never;

export type Envelope<T> = {
  data: T;
  as_of?: string;
  flags?: Record<string, unknown>;
};

export type Ledger = Schemas['Ledger'];

export type LedgersPage = NonNullable<GetJSON<'/ledgers'>['data']>;

// Transaction summary rows (ledger / tx listings). Render success from
// the `successful` bool, NOT from `result_code`'s truthiness (0 =
// txSUCCESS is falsy in JS).
export type LedgerTransaction = Schemas['TxSummary'];

export type LedgerTransactionsResp = NonNullable<
  GetJSON<'/ledgers/{seq}/transactions'>['data']
>;

// Decoded operation. `result_code` is present only in the per-tx view;
// derive success from `=== 0`, never from truthiness.
export type TxOperation = Schemas['Operation'];

export type TxEvent = Schemas['ContractEvent'] & {
  // SPEC-GAP: the per-tx event rows carry contract_id
  // (internal/api/v1/explorer_tx.go TxEventView) but the spec's shared
  // ContractEvent schema omits it (the per-contract listing hoists it
  // to the top level instead).
  contract_id?: string;
};

// Full transaction detail (summary + decoded operations + events).
export type TxSummary = Omit<Schemas['TxDetail'], 'events'> & {
  events?: TxEvent[];
};

export type ContractEvent = Schemas['ContractEvent'];

// Per-contract recent events. `next_cursor` is the opaque composite
// keyset cursor (ledger.op_index.event_index); echo back as ?cursor=.
export type ContractResp = NonNullable<
  GetJSON<'/contracts/{contract_id}'>['data']
>;

// Account activity endpoints (ADR-0038 Phase B). `scope: "all"` =
// sourced plus incoming/participant activity.
//
// GET /v1/accounts/{id}/transactions → AccountTransactionsResp
export type AccountTransactionsResp = Schemas['AccountTransactions'];

// GET /v1/accounts/{id}/operations → AccountOperationsResp
export type AccountOperationsResp = Schemas['AccountOperations'];

export type SearchResult = NonNullable<GetJSON<'/search'>['data']>;

export type SearchKind = NonNullable<SearchResult['kind']>;

// ---------------------------------------------------------------------------
// Formatting helpers (explorer-local — the wider site uses @/lib/format
// for prices; these cover hashes / stroops / ledger-relative time).
// ---------------------------------------------------------------------------

// XLM-denominated amounts come from the API as stroop integers
// (string). 1 XLM = 1e7 stroops. Render with up to 7 dp, trimming
// trailing zeros, with thousands separators on the whole part.
//
// total_coins (~1.05e18 stroops) is ~117× past 2^53, so parsing a
// string amount through Number() loses precision (ADR-0003). We
// BigInt-divide the integer stroop string instead; the Number()
// fast-path is reserved for values that arrive as JS numbers (fees,
// base_reserve — all provably < 2^53).
const STROOPS_PER_XLM = 10_000_000n;

export function stroopsToXlm(raw: string | number | null | undefined): string {
  if (raw == null || raw === '') return '—';

  // Integer stroop strings (total_coins / fee_pool) — divide with
  // BigInt so we never round a >15-digit amount through Number().
  if (typeof raw === 'string' && /^-?\d+$/.test(raw.trim())) {
    return bigStroopsToXlm(BigInt(raw.trim()));
  }

  // JS-number amounts are capped well below 2^53 by the API (fees,
  // base_reserve) — the float path is exact for them.
  let n: number;
  try {
    n = typeof raw === 'number' ? raw : Number(raw);
  } catch {
    return String(raw);
  }
  if (!Number.isFinite(n)) return String(raw);
  const xlm = n / 1e7;
  // Up to 7 dp, trimmed. toLocaleString handles thousands grouping.
  const s = xlm.toLocaleString('en-US', {
    minimumFractionDigits: 0,
    maximumFractionDigits: 7,
  });
  return s;
}

// bigStroopsToXlm formats an exact stroop BigInt as XLM: integer part
// with thousands separators + up to 7 fractional digits (trailing
// zeros trimmed). No float involved, so arbitrarily large supplies
// stay faithful to the wire string.
function bigStroopsToXlm(stroops: bigint): string {
  const neg = stroops < 0n;
  const abs = neg ? -stroops : stroops;
  const whole = abs / STROOPS_PER_XLM;
  const frac = abs % STROOPS_PER_XLM;

  const wholeStr = whole.toLocaleString('en-US');
  let out = wholeStr;
  if (frac > 0n) {
    // Pad to 7 digits, then trim trailing zeros.
    const fracStr = frac.toString().padStart(7, '0').replace(/0+$/, '');
    out = `${wholeStr}.${fracStr}`;
  }
  return neg ? `-${out}` : out;
}

export function shortHash(
  h: string | undefined | null,
  head = 8,
  tail = 8,
): string {
  if (!h) return '—';
  if (h.length <= head + tail + 1) return h;
  return `${h.slice(0, head)}…${h.slice(-tail)}`;
}

export function formatTimestamp(iso: string | undefined | null): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toISOString().replace('T', ' ').slice(0, 19) + ' UTC';
}

export function relativeAge(iso: string | undefined | null): string {
  if (!iso) return '—';
  const ms = Date.now() - new Date(iso).getTime();
  if (!Number.isFinite(ms)) return '—';
  if (ms < 0) return 'now';
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  if (s < 86_400) return `${Math.round(s / 3600)}h ago`;
  return `${Math.round(s / 86_400)}d ago`;
}

// ---------------------------------------------------------------------------
// CopyHash — monospace truncated identifier with a copy-to-clipboard
// affordance, matching the look of the rest of the explorer. The full
// value is the title attribute so hover reveals it.
// ---------------------------------------------------------------------------

export function CopyHash({
  value,
  head = 8,
  tail = 8,
  className,
}: {
  value: string | undefined | null;
  head?: number;
  tail?: number;
  className?: string;
}) {
  if (!value)
    return <span className="text-ink-faint">—</span>;
  return (
    <span className={`inline-flex items-center gap-1 ${className ?? ''}`}>
      <span className="font-mono" title={value}>
        {shortHash(value, head, tail)}
      </span>
      <CopyValue value={value} />
    </span>
  );
}

// CopyValue — bare copy-to-clipboard icon button with no rendered
// text. Use when the value is already shown next to it (e.g. an
// account link) and you just want a copy affordance.
export function CopyValue({ value }: { value: string }) {
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return;
    const t = setTimeout(() => setCopied(false), 1400);
    return () => clearTimeout(t);
  }, [copied]);
  return (
    <button
      type="button"
      onClick={async (e) => {
        e.preventDefault();
        e.stopPropagation();
        try {
          await navigator.clipboard.writeText(value);
          setCopied(true);
        } catch {
          // clipboard unavailable (insecure context) — no-op
        }
      }}
      className="text-ink-faint hover:text-brand-600"
      aria-label="Copy to clipboard"
      title="Copy to clipboard"
    >
      {copied ? (
        <Check className="h-3 w-3 text-up-strong" />
      ) : (
        <Copy className="h-3 w-3" />
      )}
    </button>
  );
}
