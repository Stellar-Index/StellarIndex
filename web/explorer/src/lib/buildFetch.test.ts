import { describe, expect, it } from 'vitest';

import { throttleDelayMs } from './buildFetch';

// A 429 is the server asking us to slow down, not a transport failure. These
// pin the wait policy: one launch-rehearsal export died on HTTP 429 from our
// own rate limiter after burning its 5 transport attempts in ~10s of linear
// backoff (it passed on retry — the tell that nothing was actually broken, we
// just asked too fast).
describe('throttleDelayMs', () => {
  it('prefers a numeric Retry-After, in seconds', () => {
    expect(throttleDelayMs('2', 0)).toBe(2_000);
    expect(throttleDelayMs('0', 3)).toBe(0);
  });

  it('accepts an HTTP-date Retry-After', () => {
    const inTen = new Date(Date.now() + 10_000).toUTCString();
    const got = throttleDelayMs(inTen, 0);
    // Second-granularity date parsing, so allow a little slack.
    expect(got).toBeGreaterThan(8_000);
    expect(got).toBeLessThanOrEqual(10_000);
  });

  it('treats an already-elapsed Retry-After date as no wait', () => {
    const past = new Date(Date.now() - 5_000).toUTCString();
    expect(throttleDelayMs(past, 0)).toBe(0);
  });

  it('caps a hostile or misconfigured Retry-After', () => {
    // Without a cap this stalls a build for hours.
    expect(throttleDelayMs('86400', 0)).toBe(60_000);
  });

  it('ignores an unparseable Retry-After and falls back to backoff', () => {
    const got = throttleDelayMs('soon-ish', 0);
    expect(got).toBeGreaterThanOrEqual(1_000);
    expect(got).toBeLessThan(1_600);
  });

  it('backs off exponentially, capped, when no Retry-After is given', () => {
    expect(throttleDelayMs(null, 0)).toBeGreaterThanOrEqual(1_000);
    expect(throttleDelayMs(null, 1)).toBeGreaterThanOrEqual(2_000);
    expect(throttleDelayMs(null, 2)).toBeGreaterThanOrEqual(4_000);
    expect(throttleDelayMs(null, 3)).toBeGreaterThanOrEqual(8_000);
    // Capped — an unbounded 2**n would reach hours by the 8th wait.
    expect(throttleDelayMs(null, 20)).toBeLessThanOrEqual(30_500);
  });

  it('jitters the fallback so fanned-out pages do not resynchronise', () => {
    const samples = new Set(Array.from({ length: 40 }, () => throttleDelayMs(null, 0)));
    expect(samples.size).toBeGreaterThan(1);
  });

  it('waits long enough in total to outlast a rate-limit window', () => {
    // 8 waits of capped exponential backoff — the budget the fetch loop
    // allows. Must comfortably exceed the anonymous tier's fixed window,
    // otherwise a build dies inside the very window it is waiting out.
    const total = Array.from({ length: 8 }, (_, i) => throttleDelayMs(null, i)).reduce(
      (a, b) => a + b,
      0,
    );
    expect(total).toBeGreaterThan(90_000);
  });
});
