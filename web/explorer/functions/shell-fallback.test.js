// @vitest-environment node
//
// These are Cloudflare Pages edge functions — no DOM involved. Running them
// under jsdom (the suite default) hits a jsdom-vs-native-fetch realm
// mismatch: jsdom's Request/AbortSignal reject a same-shaped instance that
// didn't originate from jsdom's own fetch classes. `node` uses the
// platform's native fetch (undici) throughout, matching the CF Workers
// runtime these functions actually run under.
import { describe, it, expect } from 'vitest';

import { onRequest as accountsOnRequest } from './accounts/[[path]].js';
import { onRequest as contractsOnRequest } from './contracts/[[path]].js';
import { onRequest as issuersOnRequest } from './issuers/[[path]].js';
import { onRequest as ledgersOnRequest } from './ledgers/[[path]].js';
import { onRequest as marketsOnRequest } from './markets/[[path]].js';
import { onRequest as transactionsOnRequest } from './transactions/[[path]].js';

// REL-02 (+ the sibling absence finding on contracts/issuers/ledgers): every
// one of these functions used to hardcode `status: 200` on the shell
// response regardless of whether the shell fetch itself succeeded, turning
// a missing/broken shell asset into a soft-200 "error page" that caches,
// uptime monitors, and search engines can't distinguish from a real page.

const cases = [
  ['accounts', accountsOnRequest],
  ['contracts', contractsOnRequest],
  ['issuers', issuersOnRequest],
  ['ledgers', ledgersOnRequest],
  ['markets', marketsOnRequest],
  ['transactions', transactionsOnRequest],
];

function makeContext({ shellStatus }) {
  const request = new Request('https://stellarindex.io/whatever/long-tail-id');
  return {
    request,
    env: {
      ASSETS: {
        fetch: async (req) => {
          const url = typeof req === 'string' ? req : req.url;
          if (url.includes('/shell/')) {
            return new Response('<html>shell</html>', {
              status: shellStatus,
              headers: { 'content-type': 'text/html' },
            });
          }
          // The direct-asset lookup always misses for a long-tail id, so
          // every case falls through to the shell fetch above.
          return new Response('not found', { status: 404 });
        },
      },
    },
  };
}

describe.each(cases)('%s/[[path]].js shell fallback', (_name, onRequest) => {
  it('returns 200 when the shell fetch succeeds', async () => {
    const res = await onRequest(makeContext({ shellStatus: 200 }));
    expect(res.status).toBe(200);
  });

  it('propagates 503 (not a masked 200) when the shell fetch itself fails', async () => {
    const res = await onRequest(makeContext({ shellStatus: 500 }));
    expect(res.status).toBe(503);
  });
});

// The shell sub-fetch must NOT inherit the client's conditional-request
// headers.
//
// `new Request(url, request)` copies every header, including if-none-match
// and if-modified-since — validators that describe the LONG-TAIL url the
// client asked for, not the shell asset being read. If one ever matched the
// shell's own validator, the asset server answers 304 with a null body,
// `shell.ok` is false (304 is outside 200-299), and the handler turns a
// healthy cache revalidation into a 503 with an empty body — on the second
// and every later visit to that url, including every search-engine recrawl.
//
// Latent rather than live: production emits no ETag on these routes today
// (verified 2026-08-04), so a client has no validator to send. That is a
// property of the platform's current behaviour, not a guarantee this handler
// makes — hence the unconditional sub-fetch, and this test.
describe.each(cases)('%s/[[path]].js conditional requests', (_name, onRequest) => {
  function contextRecordingShellHeaders(seen) {
    const request = new Request('https://stellarindex.io/whatever/long-tail-id', {
      headers: {
        'if-none-match': '"some-etag-for-a-different-resource"',
        'if-modified-since': 'Wed, 21 Oct 2026 07:28:00 GMT',
      },
    });
    return {
      request,
      env: {
        ASSETS: {
          fetch: async (req) => {
            const url = typeof req === 'string' ? req : req.url;
            if (url.includes('/shell/')) {
              seen.ifNoneMatch = req.headers.get('if-none-match');
              seen.ifModifiedSince = req.headers.get('if-modified-since');
              // A conforming asset server would answer 304 here if the
              // validator matched. Assert we never let it get the chance.
              return new Response('<html>shell</html>', {
                status: 200,
                headers: { 'content-type': 'text/html' },
              });
            }
            return new Response('not found', { status: 404 });
          },
        },
      },
    };
  }

  it('strips validators from the shell sub-fetch', async () => {
    const seen = {};
    const res = await onRequest(contextRecordingShellHeaders(seen));
    expect(seen.ifNoneMatch).toBeNull();
    expect(seen.ifModifiedSince).toBeNull();
    expect(res.status).toBe(200);
  });
});

// Each handler must read ITS OWN shell. These six files are copy-pasted from
// one another, and the pre-existing fake matched on `/shell/` alone — so a
// handler that fetched a sibling's shell passed every assertion.
describe('each handler fetches its own shell path', () => {
  it.each(cases)('%s', async (name, onRequest) => {
    let fetched = null;
    const ctx = {
      request: new Request('https://stellarindex.io/whatever/long-tail-id'),
      env: {
        ASSETS: {
          fetch: async (req) => {
            const url = typeof req === 'string' ? req : req.url;
            if (url.includes('/shell/')) {
              fetched = new URL(url).pathname;
              return new Response('<html>shell</html>', { status: 200 });
            }
            return new Response('not found', { status: 404 });
          },
        },
      },
    };
    await onRequest(ctx);
    expect(fetched).toBe(`/${name}/shell/`);
  });
});
