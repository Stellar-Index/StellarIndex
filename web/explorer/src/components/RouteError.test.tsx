// Regression suite for T298: RouteError caught a segment throw and did
// `console.error` only — nothing left the browser, so a production error
// was invisible to anyone but the one user whose tab happened to hit it.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render } from '@testing-library/react';

import { RouteError } from './RouteError';

function stubSendBeacon() {
  const beacon = vi.fn((_url: string, _data?: BodyInit | null) => true);
  Object.defineProperty(window.navigator, 'sendBeacon', {
    value: beacon,
    configurable: true,
    writable: true,
  });
  return beacon;
}

describe('RouteError — client-side error telemetry (T298)', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('beacons the caught error to /client-errors, not just the console', () => {
    const beacon = stubSendBeacon();
    const error = Object.assign(new Error('chart library crash'), {
      digest: 'dgst-42',
    });

    render(<RouteError error={error} reset={() => {}} section="markets" />);

    expect(beacon).toHaveBeenCalledTimes(1);
    const [url, blob] = beacon.mock.calls[0];
    expect(url).toBe('/client-errors');
    expect(blob).toBeInstanceOf(Blob);
  });

  it('the beaconed payload carries the real message, digest and section', async () => {
    const beacon = stubSendBeacon();
    const error = Object.assign(new Error('chart library crash'), {
      digest: 'dgst-42',
    });

    render(<RouteError error={error} reset={() => {}} section="markets" />);

    const blob = beacon.mock.calls[0][1] as Blob;
    const parsed = JSON.parse(await blob.text());
    expect(parsed).toMatchObject({
      message: 'chart library crash',
      digest: 'dgst-42',
      section: 'markets',
    });
  });
});
