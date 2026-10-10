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

const utc = (y: number, m: number) => Date.UTC(y, m, 1) / 1000;
const newAccounts = (p: HistoryPoint) => p.new_accounts;

describe('monthlyLine', () => {
  // The endpoint emits NO point for a quiet month; handing the served
  // points to the chart would draw a value nobody published across the hole.
  it('breaks the line at a quiet month instead of drawing through it', () => {
    const line = monthlyLine(
      [point('2024-01', 5), point('2024-04', 7)],
      newAccounts,
    );

    expect(line).toEqual([
      { time: JAN_2024, value: 5 },
      { time: utc(2024, 1), value: null },
      { time: utc(2024, 2), value: null },
      { time: APR_2024, value: 7 },
    ]);
  });

  it.each([
    [
      'spans a year boundary a month at a time',
      [point('2023-11', 1), point('2024-02', 2)],
      [utc(2023, 10), utc(2023, 11), utc(2024, 0), utc(2024, 1)],
      [1, null, null, 2],
    ],
    [
      'orders points that arrived out of order',
      [point('2024-03', 3), point('2024-01', 1)],
      [utc(2024, 0), utc(2024, 1), utc(2024, 2)],
      [1, null, 3],
    ],
    [
      'drops a point whose period label is not a month',
      [point('2024-01', 5), { ...point('2024-02', 9), period: 'whenever' }],
      [JAN_2024],
      [5],
    ],
  ])('%s', (_name, points, times, values) => {
    const line = monthlyLine(points, newAccounts);
    expect(line.map((p) => p.time)).toEqual(times);
    expect(line.map((p) => p.value)).toEqual(values);
  });

  it('reads whichever figure the caller asks for', () => {
    const line = monthlyLine([point('2024-01', 5, 41)], (p) => p.events);
    expect(line).toEqual([{ time: JAN_2024, value: 41 }]);
  });

  it('plots the points as they came rather than allocating an unbounded spine', () => {
    // Past the slot ceiling gap filling bails out; holes are drawn joined.
    const line = monthlyLine(
      [point('1900-01', 1), point('2024-01', 2)],
      newAccounts,
    );
    expect(line).toHaveLength(2);
    expect(MAX_MONTH_SLOTS).toBeGreaterThan(0);
  });
});

describe('cumulativeLine', () => {
  // Unlike monthlyLine: a running total that gained nothing is not unknown,
  // so carrying it forward states a fact.
  it('carries the total across a quiet month unbroken', () => {
    const line = cumulativeLine(
      [point('2024-01', 5), point('2024-04', 7)],
      newAccounts,
    );
    expect(line.map((p) => p.value)).toEqual([5, 5, 5, 12]);
  });

  it('ends at the sum of every point', () => {
    const points = [
      point('2024-01', 5),
      point('2024-02', 4),
      point('2024-03', 1),
    ];
    const line = cumulativeLine(points, newAccounts);
    expect(line[line.length - 1].value).toBe(10);
  });
});

it.each([
  ['monthlyLine', monthlyLine],
  ['cumulativeLine', cumulativeLine],
])('%s has nothing to draw for an empty series', (_name, fn) => {
  expect(fn([], newAccounts)).toEqual([]);
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

const accounts = (rows: GraphEdge[]) => rows.map((e) => e.account);

describe('sortEdges', () => {
  it('counts events from whichever relation the edge belongs to', () => {
    expect(edgeEvents(edge({ creations: 3 }))).toBe(3);
    expect(edgeEvents(edge({ sponsorships_started: 7 }))).toBe(7);
    expect(edgeEvents(edge({}))).toBe(0);
  });

  // funded_stroops is an exact decimal string (ADR-0003); routing it through
  // Number() ties anything past 2^53 and mis-orders the biggest funders.
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

    expect(accounts(sortEdges([smaller, larger], 'funded', 'desc'))).toEqual([
      'GLARGE',
      'GSMALL',
    ]);
    expect(accounts(sortEdges([smaller, larger], 'funded', 'asc'))).toEqual([
      'GSMALL',
      'GLARGE',
    ]);
  });

  it('treats an absent or unparsable funded figure as zero rather than throwing', () => {
    const rows = [
      edge({ account: 'GNONE' }),
      edge({ account: 'GJUNK', funded_stroops: 'not-a-number' }),
      edge({ account: 'GREAL', funded_stroops: '10' }),
    ];
    expect(sortEdges(rows, 'funded', 'desc')[0].account).toBe('GREAL');
  });

  const byColumn = [
    edge({ account: 'GB', creations: 1, first_ledger: 30, last_ledger: 90 }),
    edge({ account: 'GA', creations: 9, first_ledger: 10, last_ledger: 20 }),
    edge({ account: 'GC', creations: 5, first_ledger: 20, last_ledger: 50 }),
  ];
  it.each([
    ['events', 'desc', ['GA', 'GC', 'GB']],
    ['account', 'asc', ['GA', 'GB', 'GC']],
    ['first', 'asc', ['GA', 'GC', 'GB']],
    ['last', 'desc', ['GB', 'GC', 'GA']],
  ] as const)('sorts by %s %s', (key, dir, expected) => {
    expect(accounts(sortEdges(byColumn, key, dir))).toEqual(expected);
  });

  it('leaves the served order alone for rows it has no reason to move', () => {
    const rows = [
      edge({ account: 'GFIRST', creations: 2 }),
      edge({ account: 'GSECOND', creations: 2 }),
    ];
    expect(accounts(sortEdges(rows, 'events', 'desc'))).toEqual([
      'GFIRST',
      'GSECOND',
    ]);
  });

  it('does not mutate the array it was given', () => {
    const rows = [edge({ account: 'GB' }), edge({ account: 'GA' })];
    sortEdges(rows, 'account', 'asc');
    expect(accounts(rows)).toEqual(['GB', 'GA']);
  });
});

// graph/history 404s on older API builds; that must stay distinguishable
// from the 503 "warming" case.
describe('errorStatus', () => {
  it.each([
    ['404 Not Found on /v1/accounts/G…/graph/history', 404],
    ['503 Service Unavailable on /v1/accounts/G…/graph — warming', 503],
  ])('recovers the status the client encoded in %j', (message, status) => {
    expect(errorStatus(new Error(message))).toBe(status);
  });

  it('is null when the failure was not an HTTP one', () => {
    expect(errorStatus(new Error('Request timed out'))).toBeNull();
    expect(errorStatus('404')).toBeNull();
    expect(errorStatus(undefined)).toBeNull();
  });
});

describe('isAccountId', () => {
  const VALID = 'GDB3RSSWTUXO7MBTNMHUP3DRBIUR3QRV2CVFRAKMN4GM2B4QNGEUT6CU';
  it.each([
    [VALID, true],
    // G-strkeys are case-SENSITIVE and never lowercased in the explorer.
    [VALID.toLowerCase(), false],
    // The static-export shell sentinel and the pre-hydration empty string.
    ['shell', false],
    ['', false],
    // A contract id, not an account.
    ['CADR6Q2UOCDJAGXMAB2E6SRT35STLZ2IGLZUCXJQG7TC2LNKCU5RTQVY', false],
  ])('isAccountId(%j) is %s', (id, ok) => {
    expect(isAccountId(id)).toBe(ok);
  });
});
