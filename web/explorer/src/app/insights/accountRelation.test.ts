import { describe, expect, it } from 'vitest';

import {
  MAX_MONTH_SLOTS,
  cumulativeLine,
  edgeEvents,
  errorStatus,
  isAccountId,
  monthIndex,
  monthTime,
  monthlyLine,
  sortEdges,
  type GraphEdge,
  type HistoryPoint,
} from './accountRelation';

function point(
  period: string,
  newAccounts: number,
  events = newAccounts,
): HistoryPoint {
  return {
    period,
    period_start: `${period}-01T00:00:00Z`,
    new_accounts: newAccounts,
    events,
  };
}

// 2024-01-01T00:00:00Z / 2024-04-01T00:00:00Z in Unix seconds.
const JAN_2024 = Date.UTC(2024, 0, 1) / 1000;
const APR_2024 = Date.UTC(2024, 3, 1) / 1000;

describe('month arithmetic', () => {
  it('reads a YYYY-MM label and refuses anything else', () => {
    expect(monthIndex('2024-01')).toBe(2024 * 12);
    expect(monthIndex('2024-12')).toBe(2024 * 12 + 11);
    expect(monthIndex('2024-00')).toBeNull();
    expect(monthIndex('2024-13')).toBeNull();
    expect(monthIndex('2024-1')).toBeNull();
    expect(monthIndex('')).toBeNull();
  });

  it('maps a month index back to the first instant of that month, UTC', () => {
    expect(monthTime(monthIndex('2024-01') as number)).toBe(JAN_2024);
    expect(monthTime(monthIndex('2024-04') as number)).toBe(APR_2024);
  });
});

describe('monthlyLine', () => {
  /**
   * THE FINDING THIS GUARDS. The endpoint emits NO point for a month
   * with no activity. Handing the served points straight to the chart
   * joins the two sides of the hole with a straight line — a value
   * nobody published, drawn at the same confidence as the ones that
   * were. The quiet months must arrive as gap slots instead.
   */
  it('breaks the line at a quiet month instead of drawing through it', () => {
    const line = monthlyLine(
      [point('2024-01', 5), point('2024-04', 7)],
      (p) => p.new_accounts,
    );

    expect(line).toEqual([
      { time: JAN_2024, value: 5 },
      { time: Date.UTC(2024, 1, 1) / 1000, value: null },
      { time: Date.UTC(2024, 2, 1) / 1000, value: null },
      { time: APR_2024, value: 7 },
    ]);
    // Nothing was interpolated into the hole: no slot between the two
    // served months carries a number at all.
    expect(line.slice(1, 3).every((p) => p.value === null)).toBe(true);
  });

  it('spans a year boundary a month at a time', () => {
    const line = monthlyLine(
      [point('2023-11', 1), point('2024-02', 2)],
      (p) => p.new_accounts,
    );
    expect(line.map((p) => p.time)).toEqual([
      Date.UTC(2023, 10, 1) / 1000,
      Date.UTC(2023, 11, 1) / 1000,
      Date.UTC(2024, 0, 1) / 1000,
      Date.UTC(2024, 1, 1) / 1000,
    ]);
    expect(line.map((p) => p.value)).toEqual([1, null, null, 2]);
  });

  it('orders points that arrived out of order', () => {
    const line = monthlyLine(
      [point('2024-03', 3), point('2024-01', 1)],
      (p) => p.new_accounts,
    );
    expect(line.map((p) => p.value)).toEqual([1, null, 3]);
  });

  it('reads whichever figure the caller asks for', () => {
    const line = monthlyLine([point('2024-01', 5, 41)], (p) => p.events);
    expect(line).toEqual([{ time: JAN_2024, value: 41 }]);
  });

  it('drops a point whose period label is not a month', () => {
    const line = monthlyLine(
      [point('2024-01', 5), { ...point('2024-02', 9), period: 'whenever' }],
      (p) => p.new_accounts,
    );
    expect(line).toEqual([{ time: JAN_2024, value: 5 }]);
  });

  it('plots the points as they came rather than allocating an unbounded spine', () => {
    // A span past the slot ceiling: the gap filling bails out, and the
    // only casualty is that the holes are drawn joined.
    const line = monthlyLine(
      [point('1900-01', 1), point('2024-01', 2)],
      (p) => p.new_accounts,
    );
    expect(line).toHaveLength(2);
    expect(MAX_MONTH_SLOTS).toBeGreaterThan(0);
  });

  it('has nothing to draw for an empty series', () => {
    expect(monthlyLine([], (p) => p.new_accounts)).toEqual([]);
  });
});

describe('cumulativeLine', () => {
  /**
   * The deliberate exception to the rule above. A running total that
   * gained nothing in a month has not become UNKNOWN in that month — the
   * monthly counts are exact and sum to the whole-history total, so
   * carrying the total forward states a fact. Breaking the line here
   * would claim the opposite.
   */
  it('carries the total across a quiet month unbroken', () => {
    const line = cumulativeLine(
      [point('2024-01', 5), point('2024-04', 7)],
      (p) => p.new_accounts,
    );
    expect(line.map((p) => p.value)).toEqual([5, 5, 5, 12]);
    expect(line.some((p) => p.value === null)).toBe(false);
  });

  it('ends at the sum of every point', () => {
    const points = [
      point('2024-01', 5),
      point('2024-02', 4),
      point('2024-03', 1),
    ];
    const line = cumulativeLine(points, (p) => p.new_accounts);
    expect(line[line.length - 1].value).toBe(10);
  });

  it('has nothing to draw for an empty series', () => {
    expect(cumulativeLine([], (p) => p.new_accounts)).toEqual([]);
  });
});

function edge(over: Partial<GraphEdge>): GraphEdge {
  return {
    account: 'GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA',
    first_ledger: 1,
    last_ledger: 2,
    first_at: '2024-01-01T00:00:00Z',
    last_at: '2024-01-02T00:00:00Z',
    ...over,
  };
}

describe('sortEdges', () => {
  it('counts events from whichever relation the edge belongs to', () => {
    expect(edgeEvents(edge({ creations: 3 }))).toBe(3);
    expect(edgeEvents(edge({ sponsorships_started: 7 }))).toBe(7);
    expect(edgeEvents(edge({}))).toBe(0);
  });

  /**
   * THE FINDING THIS GUARDS. funded_stroops is an exact decimal string
   * (ADR-0003). A comparator that routed it through Number() would round
   * anything past 2^53 to the same float and then leave two genuinely
   * different rows in whatever order they arrived — silently mis-ordering
   * exactly the biggest funders the sort exists to surface. 9.0072e15
   * stroops is ~900M XLM, which real creators on this board exceed.
   */
  it('orders funded stroops exactly, past the float-precision ceiling', () => {
    const smaller = edge({
      account: 'GSMALL',
      funded_stroops: '9007199254740992',
    });
    const larger = edge({
      account: 'GLARGE',
      funded_stroops: '9007199254740993',
    });
    expect(Number(smaller.funded_stroops)).toBe(Number(larger.funded_stroops));

    const desc = sortEdges([smaller, larger], 'funded', 'desc');
    expect(desc.map((e) => e.account)).toEqual(['GLARGE', 'GSMALL']);
    const asc = sortEdges([smaller, larger], 'funded', 'asc');
    expect(asc.map((e) => e.account)).toEqual(['GSMALL', 'GLARGE']);
  });

  it('treats an absent or unparsable funded figure as zero rather than throwing', () => {
    const rows = [
      edge({ account: 'GNONE' }),
      edge({ account: 'GJUNK', funded_stroops: 'not-a-number' }),
      edge({ account: 'GREAL', funded_stroops: '10' }),
    ];
    expect(sortEdges(rows, 'funded', 'desc')[0].account).toBe('GREAL');
  });

  it('sorts by event count, account id and ledger span in both directions', () => {
    const rows = [
      edge({ account: 'GB', creations: 1, first_ledger: 30, last_ledger: 90 }),
      edge({ account: 'GA', creations: 9, first_ledger: 10, last_ledger: 20 }),
      edge({ account: 'GC', creations: 5, first_ledger: 20, last_ledger: 50 }),
    ];
    expect(sortEdges(rows, 'events', 'desc').map((e) => e.account)).toEqual([
      'GA',
      'GC',
      'GB',
    ]);
    expect(sortEdges(rows, 'account', 'asc').map((e) => e.account)).toEqual([
      'GA',
      'GB',
      'GC',
    ]);
    expect(sortEdges(rows, 'first', 'asc').map((e) => e.account)).toEqual([
      'GA',
      'GC',
      'GB',
    ]);
    expect(sortEdges(rows, 'last', 'desc').map((e) => e.account)).toEqual([
      'GB',
      'GC',
      'GA',
    ]);
  });

  it('leaves the served order alone for rows it has no reason to move', () => {
    const rows = [
      edge({ account: 'GFIRST', creations: 2 }),
      edge({ account: 'GSECOND', creations: 2 }),
    ];
    expect(sortEdges(rows, 'events', 'desc').map((e) => e.account)).toEqual([
      'GFIRST',
      'GSECOND',
    ]);
  });

  it('does not mutate the array it was given', () => {
    const rows = [edge({ account: 'GB' }), edge({ account: 'GA' })];
    sortEdges(rows, 'account', 'asc');
    expect(rows.map((e) => e.account)).toEqual(['GB', 'GA']);
  });
});

describe('errorStatus', () => {
  /**
   * THE FINDING THIS GUARDS. `graph/history` is newer than the API build
   * on some deployments, where it 404s. Treating that as the sibling
   * "warming" 503 would tell a reader to retry for a rollup cycle that
   * has already run — the endpoint has to ship first. The three cases
   * have to stay distinguishable.
   */
  it('recovers the status the client encoded in the message', () => {
    expect(
      errorStatus(new Error('404 Not Found on /v1/accounts/G…/graph/history')),
    ).toBe(404);
    expect(
      errorStatus(
        new Error('503 Service Unavailable on /v1/accounts/G…/graph — warming'),
      ),
    ).toBe(503);
  });

  it('is null when the failure was not an HTTP one', () => {
    expect(errorStatus(new Error('Request timed out'))).toBeNull();
    expect(errorStatus('404')).toBeNull();
    expect(errorStatus(undefined)).toBeNull();
  });
});

describe('isAccountId', () => {
  it('accepts a real G-strkey and refuses near-misses', () => {
    expect(
      isAccountId('GDB3RSSWTUXO7MBTNMHUP3DRBIUR3QRV2CVFRAKMN4GM2B4QNGEUT6CU'),
    ).toBe(true);
    // Lowercase: G-strkeys are case-SENSITIVE base32 and are never
    // lowercased anywhere in the explorer.
    expect(
      isAccountId('gdb3rsswtuxo7mbtnmhup3drbiur3qrv2cvfrakmn4gm2b4qngeut6cu'),
    ).toBe(false);
    // The shell sentinel the static export builds, and the empty string
    // the server render sees before hydration.
    expect(isAccountId('shell')).toBe(false);
    expect(isAccountId('')).toBe(false);
    // A contract id, not an account.
    expect(
      isAccountId('CADR6Q2UOCDJAGXMAB2E6SRT35STLZ2IGLZUCXJQG7TC2LNKCU5RTQVY'),
    ).toBe(false);
  });
});
