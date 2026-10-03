import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { pollWhileVisible } from './visiblePoll';

describe('pollWhileVisible', () => {
  let hidden = false;
  beforeEach(() => {
    vi.useFakeTimers();
    hidden = false;
    vi.spyOn(document, 'hidden', 'get').mockImplementation(() => hidden);
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it('ticks on the interval while visible', () => {
    const tick = vi.fn();
    const stop = pollWhileVisible(tick, 1000);
    vi.advanceTimersByTime(3000);
    expect(tick).toHaveBeenCalledTimes(3);
    stop();
  });

  it('skips ticks while hidden and catches up on visibilitychange', () => {
    const tick = vi.fn();
    const stop = pollWhileVisible(tick, 1000);
    hidden = true;
    vi.advanceTimersByTime(5000);
    expect(tick).not.toHaveBeenCalled();
    document.dispatchEvent(new Event('visibilitychange'));
    expect(tick).not.toHaveBeenCalled();
    hidden = false;
    document.dispatchEvent(new Event('visibilitychange'));
    expect(tick).toHaveBeenCalledTimes(1);
    stop();
  });

  it('does not add a tick when hidden and shown within one interval', () => {
    const tick = vi.fn();
    const stop = pollWhileVisible(tick, 1000);
    hidden = true;
    document.dispatchEvent(new Event('visibilitychange'));
    vi.advanceTimersByTime(300);
    hidden = false;
    document.dispatchEvent(new Event('visibilitychange'));
    expect(tick).not.toHaveBeenCalled();
    stop();
  });

  it('catches up exactly once after hiding across several intervals', () => {
    const tick = vi.fn();
    const stop = pollWhileVisible(tick, 1000);
    hidden = true;
    vi.advanceTimersByTime(3500);
    hidden = false;
    document.dispatchEvent(new Event('visibilitychange'));
    document.dispatchEvent(new Event('visibilitychange'));
    expect(tick).toHaveBeenCalledTimes(1);
    stop();
  });

  it('stops ticking and listening after cleanup', () => {
    const tick = vi.fn();
    const stop = pollWhileVisible(tick, 1000);
    stop();
    vi.advanceTimersByTime(3000);
    document.dispatchEvent(new Event('visibilitychange'));
    expect(tick).not.toHaveBeenCalled();
  });
});
