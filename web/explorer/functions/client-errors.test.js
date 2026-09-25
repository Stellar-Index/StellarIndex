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

  it('413s on a declared Content-Length over the cap without reading the body', async () => {
    const stream = countingStream(64, 1024);
    const res = await onRequestPost(
      streamContext(stream.body, { 'content-length': String(64 * 1024) }),
    );
    expect(res.status).toBe(413);
    expect(stream.bytesPulled()).toBe(0);
  });

  it('stops reading an undeclared body at the cap instead of buffering it whole', async () => {
    const chunkSize = 1024;
    const stream = countingStream(1024, chunkSize);
    const res = await onRequestPost(streamContext(stream.body));
    expect(res.status).toBe(413);
    // The cap is 4096 bytes; the stream may pull at most one chunk past it
    // (plus one readahead), never the full 1 MiB.
    expect(stream.bytesPulled()).toBeLessThanOrEqual(4096 + 2 * chunkSize);
  });

  it('measures the cap in bytes, not UTF-16 code units', async () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
    // 2000 code units, 6000 UTF-8 bytes.
    const res = await onRequestPost(
      makeContext({ message: '\u20ac'.repeat(2000) }),
    );
    expect(res.status).toBe(413);
    expect(spy).not.toHaveBeenCalled();
  });

  it('decodes a multi-byte body under the cap intact', async () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const res = await onRequestPost(
      makeContext({ message: 'd\u00e9j\u00e0 \u20ac' }),
    );
    expect(res.status).toBe(204);
    expect(spy).toHaveBeenCalledWith(
      '[client-error]',
      expect.objectContaining({ message: 'd\u00e9j\u00e0 \u20ac' }),
    );
  });
});

function countingStream(chunks, chunkSize) {
  let pulled = 0;
  let emitted = 0;
  const chunk = new TextEncoder().encode('x'.repeat(chunkSize));
  const body = new ReadableStream(
    {
      pull(controller) {
        if (emitted >= chunks) {
          controller.close();
          return;
        }
        emitted += 1;
        pulled += chunk.byteLength;
        controller.enqueue(chunk);
      },
    },
    { highWaterMark: 0 },
  );
  return { body, bytesPulled: () => pulled };
}

function streamContext(body, headers = {}) {
  return {
    request: new Request('https://stellarindex.io/client-errors', {
      method: 'POST',
      headers: { 'content-type': 'application/json', ...headers },
      body,
      duplex: 'half',
    }),
  };
}
