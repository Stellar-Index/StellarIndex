// @vitest-environment node
import { afterEach, describe, expect, it, vi } from 'vitest';

const { onRequestPost, onRequestGet } = await import('./client-errors.js');

function makeContext(body) {
  return {
    request: new Request('https://stellarindex.io/client-errors', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
    }),
  };
}

describe('client-errors function', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('logs a truncated, structured record and returns 204', async () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {});

    const res = await onRequestPost(
      makeContext({
        message: 'chart library crash',
        digest: 'dgst-42',
        section: 'markets',
        path: '/markets',
      }),
    );

    expect(res.status).toBe(204);
    expect(spy).toHaveBeenCalledWith(
      '[client-error]',
      expect.objectContaining({
        message: 'chart library crash',
        digest: 'dgst-42',
        section: 'markets',
        path: '/markets',
      }),
    );
  });

  it('rejects GET', async () => {
    const res = await onRequestGet();
    expect(res.status).toBe(405);
  });

  it('degrades to 204 without throwing on unparsable JSON', async () => {
    const ctx = {
      request: new Request('https://stellarindex.io/client-errors', {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: 'not json',
      }),
    };
    const res = await onRequestPost(ctx);
    expect(res.status).toBe(204);
  });

  it('413s an oversized body instead of logging it', async () => {
    const res = await onRequestPost(makeContext({ message: 'x'.repeat(5000) }));
    expect(res.status).toBe(413);
  });
});
