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

import type { components, paths } from '@/api/types';
import Link from 'next/link';
import { Fragment } from 'react';
import { AssetLink } from '@/components/AssetLink';

import { CopyButton, Fact, FactList } from '@/components/ui';
import { PANEL_LABEL_CLASS } from '@/components/ui/Page';
import { cn } from '@/lib/cn';
import { formatUnitsReadable } from '@/lib/format';

// ---------------------------------------------------------------------------
// Wire shapes — every endpoint is wrapped as { data, as_of, flags }.
//
// Derived from the generated OpenAPI contract (src/api/types.ts,
// `make web-generate-api`) so spec drift fails the build instead of
// shipping as undefined in the UI. Fields the API serves but the spec
// under-declares are locally narrowed (see per-site comments).
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

// Envelope lives in src/api/client so api code can use it; re-exported here
// for page importers.
export type { Envelope } from '@/api/client';

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

// contract_id is spec'd on ContractEvent since board #33.
export type TxEvent = Schemas['ContractEvent'];

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
// fast-path is reserved for callers passing a JS number.
const STROOPS_PER_XLM = 10_000_000n;

export function stroopsToXlm(raw: string | number | null | undefined): string {
  if (raw == null || raw === '') return '—';

  // Integer stroop strings (every API stroop amount) — divide with
  // BigInt so we never round a >15-digit amount through Number().
  if (typeof raw === 'string' && /^-?\d+$/.test(raw.trim())) {
    return bigStroopsToXlm(BigInt(raw.trim()));
  }

  // Non-integer or JS-number input: the float path, exact below 2^53.
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

/** xlmReadable — stroops at a readable precision ("50B", "12,346", "1.5"). */
export function xlmReadable(stroops: string | null | undefined): string {
  return formatUnitsReadable(stroops, 7);
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

// Classic (Stellar CAP) operation-amount fields arrive from the API as
// raw stroop integers at the protocol-fixed 7-decimal scale (ADR-0003),
// so the display layer must divide by 1e7 — via the exact BigInt path
// (stroopsToXlm), never Number(). Keyed on field name so ONLY amounts
// scale: ids, flags, addresses and base_fee (genuinely stroop-labelled)
// keep rendering verbatim.
const CLASSIC_AMOUNT_FIELDS = new Set([
  'amount', // payment / clawback / offer amount
  'starting_balance', // create_account
  'send_max',
  'send_amount',
  'dest_amount',
  'dest_min',
  'buy_amount', // manage_buy_offer
  'limit', // change_trust — the trust limit is a stroop-scaled amount
]);

// renderOpFieldValue formats one decoded operation field for display.
// A classic amount field (see CLASSIC_AMOUNT_FIELDS) is a stroop integer
// and is scaled to XLM via stroopsToXlm (exact BigInt divide); a
// non-numeric amount value falls through to the verbatim path. Every
// other field renders exactly as before — verbatim string/number/bool,
// else JSON.
export function renderOpFieldValue(key: string, v: unknown): string {
  if (v == null) return '—';
  if (key === 'price' && isPriceFraction(v)) return priceText(v);
  if (
    CLASSIC_AMOUNT_FIELDS.has(key) &&
    (typeof v === 'string' || typeof v === 'number')
  ) {
    return stroopsToXlm(v);
  }
  if (
    typeof v === 'string' ||
    typeof v === 'number' ||
    typeof v === 'boolean'
  ) {
    return String(v);
  }
  try {
    return JSON.stringify(v);
  } catch {
    return String(v);
  }
}

type PriceFraction = { n: number | string; d: number | string };

export function isPriceFraction(v: unknown): v is PriceFraction {
  if (typeof v !== 'object' || v === null) return false;
  const { n, d } = v as Record<string, unknown>;
  const ok = (x: unknown) =>
    (typeof x === 'number' && Number.isInteger(x)) ||
    (typeof x === 'string' && /^-?\d+$/.test(x));
  return ok(n) && ok(d);
}

// A Stellar price is the rational n/d. Shown to 7 decimals (one stroop),
// truncated by exact BigInt division; the fraction itself stays available
// via priceFractionText for a title.
export function priceText(p: PriceFraction): string {
  const n = BigInt(p.n);
  const d = BigInt(p.d);
  if (d === 0n) return priceFractionText(p);
  const neg = n < 0n !== d < 0n;
  const abs = (x: bigint) => (x < 0n ? -x : x);
  const scaled = (abs(n) * 10_000_000n) / abs(d);
  const digits = scaled.toString().padStart(8, '0');
  const frac = digits.slice(-7).replace(/0+$/, '');
  return `${neg ? '-' : ''}${digits.slice(0, -7)}${frac ? `.${frac}` : ''}`;
}

export function priceFractionText(p: PriceFraction): string {
  return `${p.n} / ${p.d}`;
}

// XDR OperationResultCode 0 is opINNER: an inner result is present, not
// success. The inner code names the outcome and its success variant ends
// in `_success`. Undefined when the API carried no result code.
export function opSucceeded(
  op: Pick<TxOperation, 'result_code' | 'inner_result'>,
): boolean | undefined {
  if (op.result_code == null) return undefined;
  if (op.result_code !== 0) return false;
  if (op.inner_result == null) return true;
  return op.inner_result.endsWith('_success');
}

// scaledUnits scales an exact base-unit integer STRING by 10^decimals
// for display. Scaling happens on the decimal string, so the result is
// one correctly-rounded parse of the exact value (never Number(int) /
// 10^d, which rounds twice). It is still a float, approximate beyond ~15
// significant digits: display and charts only (ADR-0003). Returns NaN
// for non-integer input so callers can fall back to the raw string.
export function scaledUnits(baseUnits: string, decimals: number): number {
  const s = baseUnits.trim();
  if (!/^-?\d+$/.test(s)) return NaN;
  const neg = s.startsWith('-');
  const digits = neg ? s.slice(1) : s;
  if (decimals <= 0) {
    const n = Number(digits + '0'.repeat(-decimals));
    return neg ? -n : n;
  }
  const padded = digits.padStart(decimals + 1, '0');
  const whole = padded.slice(0, padded.length - decimals);
  const frac = padded.slice(padded.length - decimals);
  const n = Number(`${whole}.${frac}`);
  return neg ? -n : n;
}

export function shortHash(
  h: string | undefined | null,
  head = 8,
  tail = 8,
): string {
  if (!h) return '—';
  if (h.length <= head + tail + 1) return h;
  return `${h.slice(0, head)}…${tail > 0 ? h.slice(-tail) : ''}`;
}

export function formatTimestamp(iso: string | undefined | null): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toISOString().replace('T', ' ').slice(0, 19) + ' UTC';
}

// Named re-export of lib/format's formatRelative; explorer files import it here.
export { formatRelative as relativeAge } from '@/lib/format';

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
  if (!value) return <span className="text-ink-faint">—</span>;
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
  // Canonical CopyButton: 24×24 hit area (WCAG 2.5.8) plus a "Copied" live region.
  return <CopyButton value={value} title="Copy to clipboard" />;
}

/**
 * HashLink — a truncated identifier that links to its entity. The full value
 * sits in the title and, with `copy`, behind the copy button.
 */
export function HashLink({
  href,
  value,
  head = 8,
  tail = 8,
  copy = false,
  className,
}: {
  href: string;
  value: string | undefined | null;
  head?: number;
  tail?: number;
  copy?: boolean;
  className?: string;
}) {
  if (!value) return <span className="text-ink-faint">—</span>;
  return (
    <span className={cn('inline-flex items-center gap-1.5', className)}>
      <Link
        href={href}
        className="text-ink hover:text-brand-600 font-mono text-[13px] break-all"
        title={value}
      >
        {shortHash(value, head, tail)}
      </Link>
      {copy && <CopyButton value={value} />}
    </span>
  );
}

export type AuthInvocation = {
  kind: string;
  contract_id?: string;
  function_name?: string;
  args?: string[];
  sub_invocations?: AuthInvocation[];
};

function InvocationTree({
  nodes,
  depth = 0,
}: {
  nodes: AuthInvocation[];
  depth?: number;
}) {
  return (
    <ul
      className={
        depth === 0 ? 'space-y-1' : 'border-line mt-1 space-y-1 border-l pl-3'
      }
    >
      {nodes.map((n, i) => (
        <li key={i}>
          <div className="flex flex-wrap items-baseline gap-1.5 font-mono text-xs">
            {n.kind === 'invoke_contract' ? (
              <>
                {n.contract_id && (
                  <span className="text-ink-muted" title={n.contract_id}>
                    {shortHash(n.contract_id, 6, 4)}
                  </span>
                )}
                <span className="text-brand-700 font-medium">
                  {n.function_name}
                </span>
                {n.args && n.args.length > 0 && (
                  <span className="text-ink-faint break-all">
                    ({n.args.join(', ')})
                  </span>
                )}
              </>
            ) : (
              <span className="text-ink-muted">create_contract</span>
            )}
          </div>
          {n.sub_invocations && n.sub_invocations.length > 0 && (
            <InvocationTree nodes={n.sub_invocations} depth={depth + 1} />
          )}
        </li>
      ))}
    </ul>
  );
}

/**
 * OpBody — the decoded body of one operation: every field but the raw XDR
 * as facts, Soroban authorizations as a call tree, the XDR behind a
 * disclosure. Shared by the transaction and operation pages.
 */
// Decoded fields whose value is a canonical asset id ("native", CODE-ISSUER
// or a contract address); `path` is a list of them.
const ASSET_FIELDS = new Set([
  'asset',
  'asset_a',
  'asset_b',
  'buying',
  'selling',
  'dest_asset',
  'send_asset',
]);

function OpFieldValue({ k, v }: { k: string; v: unknown }) {
  if (ASSET_FIELDS.has(k) && typeof v === 'string')
    return <AssetLink canonical={v} />;
  if (
    k === 'path' &&
    Array.isArray(v) &&
    v.every((x) => typeof x === 'string')
  ) {
    if (v.length === 0)
      return <span className="text-ink-muted">none, direct</span>;
    return (
      <span className="inline-flex flex-wrap items-center gap-x-1.5 gap-y-1">
        {(v as string[]).map((a, i) => (
          <Fragment key={`${i}-${a}`}>
            {i > 0 && (
              <span className="text-ink-faint" aria-hidden>
                →
              </span>
            )}
            <AssetLink canonical={a} />
          </Fragment>
        ))}
      </span>
    );
  }
  return <>{renderOpFieldValue(k, v)}</>;
}

export function OpBody({ op }: { op: TxOperation }) {
  const fields = op.fields ?? {};
  const keys = Object.keys(fields).filter((k) => k !== 'authorizations');
  const auth = Array.isArray(fields.authorizations)
    ? (fields.authorizations as AuthInvocation[])
    : null;
  return (
    <div className="space-y-4">
      {keys.length > 0 ? (
        <FactList columns={3}>
          {keys.map((k) => (
            <Fact
              key={k}
              label={k.replace(/_/g, ' ')}
              mono={!(ASSET_FIELDS.has(k) || k === 'path')}
              title={
                k === 'price' && isPriceFraction(fields[k])
                  ? priceFractionText(fields[k])
                  : undefined
              }
            >
              <OpFieldValue k={k} v={fields[k]} />
            </Fact>
          ))}
        </FactList>
      ) : (
        <p className="text-ink-muted text-sm">
          No decoded fields for this operation type.
        </p>
      )}
      {auth && auth.length > 0 && (
        <div>
          <div
            className={cn(PANEL_LABEL_CLASS, 'mb-1.5 cursor-help')}
            title="The nested contract calls this operation authorized (from the op's SorobanAuthorizationEntries). Not the full execution trace."
          >
            Authorized invocations
          </div>
          <InvocationTree nodes={auth} />
        </div>
      )}
      {op.raw_xdr && (
        <details>
          <summary className="text-ink-muted hover:text-ink cursor-pointer text-xs font-medium">
            Raw XDR
          </summary>
          <pre className="border-line bg-surface-muted text-ink-body mt-2 overflow-x-auto rounded-md border p-3 font-mono text-xs leading-relaxed break-all whitespace-pre-wrap">
            {op.raw_xdr}
          </pre>
        </details>
      )}
    </div>
  );
}
