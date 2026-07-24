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
