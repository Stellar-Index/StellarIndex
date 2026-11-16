import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render } from '@testing-library/react';

import GlobalError from '@/app/global-error';

import { ClientErrorListener } from './ClientErrorListener';

function stubSendBeacon() {
  const beacon = vi.fn((_url: string, _data?: BodyInit | null) => true);
  Object.defineProperty(window.navigator, 'sendBeacon', {
    value: beacon,
    configurable: true,
    writable: true,
  });
  return beacon;
}

async function payloads(beacon: ReturnType<typeof stubSendBeacon>) {
  return Promise.all(
    beacon.mock.calls.map(async ([, blob]) =>
      JSON.parse(await (blob as Blob).text()),
    ),
  );
}

function rejection(reason: unknown) {
  const e = new Event('unhandledrejection') as Event & { reason?: unknown };
  e.reason = reason;
  return e;
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

// Errors thrown outside render (event handlers, timers, stream listeners,
// un-awaited promises) reach no React boundary, so nothing reported them.
describe('ClientErrorListener', () => {
  it('reports an uncaught window error and an unhandled rejection', async () => {
    const beacon = stubSendBeacon();
    render(<ClientErrorListener />);

    window.dispatchEvent(
      new ErrorEvent('error', { message: 'handler blew up' }),
    );
    window.dispatchEvent(rejection(new Error('mint failed')));

    expect(beacon.mock.calls.map(([url]) => url)).toEqual([
      '/client-errors',
      '/client-errors',
    ]);
    expect(await payloads(beacon)).toEqual([
      expect.objectContaining({
        message: 'handler blew up',
        section: 'window-error',
      }),
      expect.objectContaining({
        message: 'mint failed',
        section: 'unhandled-rejection',
      }),
    ]);
  });

  it('reports a repeating error once and stops after five distinct ones', () => {
    const beacon = stubSendBeacon();
    render(<ClientErrorListener />);

    for (let i = 0; i < 3; i++)
      window.dispatchEvent(new ErrorEvent('error', { message: 'same' }));
    expect(beacon).toHaveBeenCalledTimes(1);

    for (let i = 0; i < 20; i++)
      window.dispatchEvent(new ErrorEvent('error', { message: `e${i}` }));
    expect(beacon).toHaveBeenCalledTimes(5);
  });

  it('does not report the opaque cross-origin "Script error."', () => {
    const beacon = stubSendBeacon();
    render(<ClientErrorListener />);
    window.dispatchEvent(new ErrorEvent('error', { message: 'Script error.' }));
    expect(beacon).not.toHaveBeenCalled();
  });

  it('stops listening when unmounted', () => {
    const beacon = stubSendBeacon();
    render(<ClientErrorListener />).unmount();
    window.dispatchEvent(new ErrorEvent('error', { message: 'late' }));
    expect(beacon).not.toHaveBeenCalled();
  });
});

// The shell crashing is what every visitor sees after a bad deploy; it was
// the one boundary that reported nothing.
describe('global-error', () => {
  it('reports the shell crash', async () => {
    const beacon = stubSendBeacon();
    vi.spyOn(console, 'error').mockImplementation(() => {});
    const error = Object.assign(new Error('root layout crashed'), {
      digest: 'dgst-7',
    });

    render(<GlobalError error={error} reset={() => {}} />);

    expect(beacon).toHaveBeenCalledTimes(1);
    expect(await payloads(beacon)).toEqual([
      expect.objectContaining({
        message: 'root layout crashed',
        digest: 'dgst-7',
        section: 'global',
      }),
    ]);
  });
});
