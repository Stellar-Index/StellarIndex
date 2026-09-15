// accountRelation — the pure half of the per-address sponsor/creator
// detail pages: the relation's vocabulary, the monthly-series geometry,
// and the page-local sort.
//
// It lives apart from the components for the reason charts/dailyGaps.ts
// does: the judgments that matter here are arithmetic, not markup, and
// the ones about ABSENCE are the easiest to get silently wrong.
//
// Two rules the series builders below exist to enforce:
//
//   A QUIET MONTH IS NOT A ZERO. GET /v1/accounts/{g}/graph/history emits
//   no point for a month with no activity — never a zero and never a
//   carried-forward value. A monthly chart therefore BREAKS at such a
//   month (a null-valued slot) rather than joining the two sides with a
//   straight line, which would draw a number nobody served at the same
//   confidence as the ones that were.
//
//   A RUNNING TOTAL IS THE ONE SERIES THAT MAY CROSS A QUIET MONTH. Its
//   value there is not an interpolation: `new_accounts` is exact and sums
//   to `totals.accounts`, so a month that added nothing leaves the total
//   where it was. Carrying it forward states a fact; breaking the line
//   would imply the total was unknown, which it is not.
//
// Both spines run first served point → last served point, so every slot
// they fill is inside the series' own coverage span, where an absent
// month means "nothing happened" rather than "nothing was observed".
import type { LinePoint } from '@/components/charts/LineChart';
import type { components } from '@/api/types';

export type Relation = 'created' | 'sponsored';

export type HistoryPoint = components['schemas']['AccountGraphHistoryPoint'];
export type HistorySeries = components['schemas']['AccountGraphHistorySeries'];
export type GraphEdge = components['schemas']['AccountGraphEdge'];

/**
 * How each relation is named, routed and counted. One table so the two
 * pages cannot drift into two vocabularies for the same graph — the
 * sponsorship arm in particular has wording that must not soften
 * ("arrangements STARTED", never "sponsorships held").
 */
export const RELATION: Record<
  Relation,
  {
    /** URL segment of the board this relation's detail page hangs off. */
    board: string;
    boardLabel: string;
    /** Singular noun for the address itself. */
    actor: string;
    /** What the outbound edge list is a list of. */
    counterparties: string;
    /** Column header for the per-edge event count. */
    eventColumn: string;
    /** What one event is, in prose. */
    eventNoun: string;
  }
> = {
  created: {
    board: '/insights/creators',
    boardLabel: 'Account creators',
    actor: 'creator',
    counterparties: 'accounts created',
    eventColumn: 'Creations',
    eventNoun: 'creation',
  },
  sponsored: {
    board: '/insights/sponsors',
    boardLabel: 'Account sponsors',
    actor: 'sponsor',
    counterparties: 'accounts sponsored',
    eventColumn: 'Started',
    eventNoun: 'sponsorship arrangement started',
  },
};

/** Stellar account ids are 56 chars: 'G' + 55 base32 alphanumerics. */
const ACCOUNT_RE = /^G[A-Z2-7]{55}$/;

export function isAccountId(value: string): boolean {
  return ACCOUNT_RE.test(value);
}

/**
 * Ceiling on how many month slots one series may occupy, mirroring
 * charts/dailyGaps.MAX_DAY_SLOTS. The server caps the POINT count; this
 * caps the SPAN, so a pair of points a century apart cannot make the gap
 * filling allocate an unbounded spine. Past it the points are plotted as
 * they came and the only casualty is that the holes are drawn joined.
 */
export const MAX_MONTH_SLOTS = 1200;

/**
 * "YYYY-MM" → months since year 0, or null when the label is not a
 * month. Indexing on the LABEL rather than parsing `period_start` keeps
 * the spine on exact calendar arithmetic: stepping a timestamp by
 * "one month" has no fixed length, and every month here is a UTC
 * calendar month by construction.
 */
export function monthIndex(period: string): number | null {
  const m = /^(\d{4})-(\d{2})$/.exec(period);
  if (!m) return null;
  const month = Number(m[2]);
  if (month < 1 || month > 12) return null;
  return Number(m[1]) * 12 + (month - 1);
}

/** A month index back to the Unix seconds of its first instant (UTC). */
export function monthTime(index: number): number {
  return Math.floor(Date.UTC(Math.floor(index / 12), index % 12, 1) / 1000);
}

type Indexed = { index: number; point: HistoryPoint };

function indexed(points: readonly HistoryPoint[]): Indexed[] {
  const out: Indexed[] = [];
  for (const point of points) {
    const index = monthIndex(point.period);
    if (index !== null) out.push({ index, point });
  }
  out.sort((a, b) => a.index - b.index);
  return out;
}

/**
 * Monthly series → chart geometry with every quiet month made EXPLICIT.
 *
 * `value` reads the figure off a point. Months between the first and
 * last served point that carry no point become gap slots (null value):
 * the slot keeps its place on the time axis and the line breaks there.
 */
export function monthlyLine(
  points: readonly HistoryPoint[],
  value: (p: HistoryPoint) => number,
): LinePoint[] {
  const rows = indexed(points);
  if (rows.length === 0) return [];
  const first = rows[0].index;
  const last = rows[rows.length - 1].index;
  if (last - first + 1 > MAX_MONTH_SLOTS) {
    return rows.map((r) => ({
      time: monthTime(r.index),
      value: value(r.point),
    }));
  }
  const byIndex = new Map(rows.map((r) => [r.index, r.point]));
  const out: LinePoint[] = [];
  for (let i = first; i <= last; i++) {
    const point = byIndex.get(i);
    out.push({ time: monthTime(i), value: point ? value(point) : null });
  }
  return out;
}

/**
 * Running total of `value` across the same spine — the one series that
 * crosses a quiet month unbroken, because a total that gained nothing
 * has not become unknown. Never null-valued, so the line is continuous
 * over the whole span.
 */
export function cumulativeLine(
  points: readonly HistoryPoint[],
  value: (p: HistoryPoint) => number,
): LinePoint[] {
  const rows = indexed(points);
  if (rows.length === 0) return [];
  const first = rows[0].index;
  const last = rows[rows.length - 1].index;
  const byIndex = new Map(rows.map((r) => [r.index, r.point]));
  let running = 0;
  if (last - first + 1 > MAX_MONTH_SLOTS) {
    return rows.map((r) => {
      running += value(r.point);
      return { time: monthTime(r.index), value: running };
    });
  }
  const out: LinePoint[] = [];
  for (let i = first; i <= last; i++) {
    const point = byIndex.get(i);
    if (point) running += value(point);
    out.push({ time: monthTime(i), value: running });
  }
  return out;
}

/**
 * The HTTP status behind an apiGet rejection, or null when the failure
 * was not an HTTP one (a timeout, an abort, a parse error).
 *
 * The client throws `${status} ${statusText} on ${path}…`, so the status
 * is recoverable — and it has to be, because the three ways this
 * surface can be absent need three different sentences. A 503 is
 * "warming, retry shortly"; a 404 is "this deployment's API predates the
 * endpoint", which no amount of retrying fixes; anything else is a
 * genuine fault. Collapsing them into one "unavailable" would have told
 * a reader to wait for a release that has to be deployed first.
 */
export function errorStatus(err: unknown): number | null {
  if (!(err instanceof Error)) return null;
  const m = /^(\d{3}) /.exec(err.message);
  return m === null ? null : Number(m[1]);
}

export type EdgeSortKey = 'account' | 'events' | 'funded' | 'first' | 'last';
export type SortDirection = 'asc' | 'desc';

/** Events behind one edge, whichever relation it belongs to. */
export function edgeEvents(edge: GraphEdge): number {
  return edge.creations ?? edge.sponsorships_started ?? 0;
}

/**
 * Stroops as a BigInt for ORDERING ONLY. The wire carries an exact
 * decimal string (ADR-0003) and every figure the table prints comes from
 * that string; routing it through Number() to sort would round a
 * >90M-XLM funding total and silently reorder the rows it was asked to
 * order. A value that does not parse sorts as 0 rather than throwing.
 */
function fundedStroops(edge: GraphEdge): bigint {
  const raw = edge.funded_stroops;
  if (!raw || !/^-?\d+$/.test(raw)) return 0n;
  return BigInt(raw);
}

function compare(a: GraphEdge, b: GraphEdge, key: EdgeSortKey): number {
  switch (key) {
    case 'account':
      return a.account < b.account ? -1 : a.account > b.account ? 1 : 0;
    case 'events':
      return edgeEvents(a) - edgeEvents(b);
    case 'funded': {
      const x = fundedStroops(a);
      const y = fundedStroops(b);
      return x < y ? -1 : x > y ? 1 : 0;
    }
    // Ledger sequence, not the timestamp: it is the exact ordinal the
    // rows were produced in, and it needs no date parsing to compare.
    case 'first':
      return a.first_ledger - b.first_ledger;
    case 'last':
      return a.last_ledger - b.last_ledger;
  }
}

/**
 * Sort the LOADED PAGE of edges. This is deliberately not a whole-set
 * ordering and the panel says so: /v1/accounts/{g}/graph is keyset-paged
 * by counterparty account id, so there is no server-side "top by
 * creations" to ask for, and the busiest sponsor's set runs to 785,615
 * accounts — a number no client may sort. Ties keep the served order
 * (Array.prototype.sort is stable), so the page never reshuffles rows it
 * has no reason to move.
 */
export function sortEdges(
  edges: readonly GraphEdge[],
  key: EdgeSortKey,
  direction: SortDirection,
): GraphEdge[] {
  const sign = direction === 'asc' ? 1 : -1;
  return [...edges].sort((a, b) => sign * compare(a, b, key));
}
