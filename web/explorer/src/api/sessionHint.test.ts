import { describe, it, expect, beforeEach } from 'vitest';

import {
  SESSION_HINT_COOKIE,
  clearSessionHint,
  readCookie,
  sessionHintPresent,
} from './sessionHint';

function wipe() {
  document.cookie = `${SESSION_HINT_COOKIE}=; Max-Age=0; Path=/`;
  document.cookie = 'unrelated=; Max-Age=0; Path=/';
  document.cookie = 'stellarindex_session_presentish=; Max-Age=0; Path=/';
}

describe('readCookie', () => {
  it('finds a value regardless of position or surrounding whitespace', () => {
    const jar = 'a=1; stellarindex_session_present=1; z=9';
    expect(readCookie(jar, SESSION_HINT_COOKIE)).toBe('1');
  });

  it('does not match a name that merely has the hint as a prefix', () => {
    const jar = 'stellarindex_session_presentish=1';
    expect(readCookie(jar, SESSION_HINT_COOKIE)).toBeNull();
  });

  it('does not match a name that merely has the hint as a suffix', () => {
    const jar = 'x_stellarindex_session_present=1';
    expect(readCookie(jar, SESSION_HINT_COOKIE)).toBeNull();
  });

  it('reads an empty value as absent', () => {
    // A cookie cleared as `name=` without an expiry would otherwise
    // look exactly like a live session.
    expect(readCookie('stellarindex_session_present=', SESSION_HINT_COOKIE)).toBeNull();
  });
});

describe('sessionHintPresent', () => {
  beforeEach(wipe);

  it('is false for a browser that has never signed in', () => {
    expect(sessionHintPresent()).toBe(false);
  });

  it('is true once the API has written the hint', () => {
    document.cookie = `${SESSION_HINT_COOKIE}=1; Path=/`;
    expect(sessionHintPresent()).toBe(true);
  });

  it('is unaffected by other cookies on the same jar', () => {
    document.cookie = 'unrelated=whatever; Path=/';
    expect(sessionHintPresent()).toBe(false);
  });
});

describe('clearSessionHint', () => {
  beforeEach(wipe);

  it('removes a stale hint so the next page load probes nothing', () => {
    document.cookie = `${SESSION_HINT_COOKIE}=1; Path=/`;
    expect(sessionHintPresent()).toBe(true);
    clearSessionHint();
    expect(sessionHintPresent()).toBe(false);
  });

  it('is a no-op when there is no hint to clear', () => {
    clearSessionHint();
    expect(sessionHintPresent()).toBe(false);
  });

  it('leaves unrelated cookies alone', () => {
    document.cookie = `${SESSION_HINT_COOKIE}=1; Path=/`;
    document.cookie = 'unrelated=keepme; Path=/';
    clearSessionHint();
    expect(readCookie(document.cookie, 'unrelated')).toBe('keepme');
  });
});
