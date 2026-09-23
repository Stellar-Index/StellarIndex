// @vitest-environment node
//
// These are Cloudflare Pages edge functions — no DOM involved. Running them
// under jsdom (the suite default) hits a jsdom-vs-native-fetch realm
// mismatch: jsdom's Request/AbortSignal reject a same-shaped instance that
// didn't originate from jsdom's own fetch classes. `node` uses the
// platform's native fetch (undici) throughout, matching the CF Workers
// runtime these functions actually run under.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { describe, it, expect } from 'vitest';

// REL-02 (+ the sibling absence finding on contracts/issuers/ledgers): every
// one of these functions used to hardcode `status: 200` on the shell
// response regardless of whether the shell fetch itself succeeded, turning
// a missing/broken shell asset into a soft-200 "error page" that caches,
// uptime monitors, and search engines can't distinguish from a real page.

// K022: `cases` used to be a hand-maintained [name, handler, path] array —
// a new long-tail shell route (the `assets/[[path]].js` handler drifted in
// untested this way) shipped with no matching entry and no failure. Walk
// the directory instead: every `[[path]].js` whose source references a
// `/shell/` sub-fetch is a shell-fallback handler and is picked up
// automatically. `og/[[path]].js` is a different contract (dynamic image
// generation, covered by its own og.test.js) and has no such reference, so
// it's excluded without needing a hand-written exclusion list either.
//
// The expected shell path is derived from the handler's DIRECTORY position,
// not grepped from its own source — grepping the source would just echo
// back a copy-pasted wrong path, defeating the "each handler fetches its
// own shell path" check below.
const functionsDir = path.dirname(fileURLToPath(import.meta.url));

function discoverShellFallbackHandlers(dir, relSegments = []) {
  const found = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    if (entry.isDirectory()) {
      found.push(
        ...discoverShellFallbackHandlers(path.join(dir, entry.name), [
          ...relSegments,
          entry.name,
        ]),
      );
      continue;
    }
    if (entry.name !== '[[path]].js') continue;
    const filePath = path.join(dir, entry.name);
    if (!fs.readFileSync(filePath, 'utf8').includes('/shell/')) continue;
    found.push({ name: relSegments.join('/'), filePath });
  }
  return found;
}

const cases = await Promise.all(
  discoverShellFallbackHandlers(functionsDir).map(
    async ({ name, filePath }) => {
      const mod = await import(pathToFileURL(filePath).href);
      return [name, mod.onRequest, `/${name}/shell/`];
    },
  ),
);

// Guard against the discovery walk itself silently finding nothing (a
// broken cwd/glob would pass every describe.each below vacuously).
if (cases.length === 0) {
  throw new Error(
    'discoverShellFallbackHandlers found no [[path]].js shell-fallback handlers — check functionsDir',
  );
}

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

// T313: `env.ASSETS.fetch` can reject (a worker-runtime fault — binding
// unavailable, network fault) instead of resolving to a Response. Every
// handler here used to have zero `try`/`catch` around it, so that rejection
// propagated out of `onRequest` as an unhandled exception instead of the
// same 503 already returned for a failed-but-resolved shell fetch.
function makeThrowingContext() {
  const request = new Request('https://stellarindex.io/whatever/long-tail-id');
  return {
    request,
    env: {
      ASSETS: {
        fetch: async () => {
          throw new Error('binding unavailable');
        },
      },
    },
  };
}

describe.each(cases)(
  '%s/[[path]].js shell fallback error handling',
  (_name, onRequest) => {
    it('returns a 503 Response instead of throwing when ASSETS.fetch rejects', async () => {
      const res = await onRequest(makeThrowingContext());
      expect(res).toBeInstanceOf(Response);
      expect(res.status).toBe(503);
    });
  },
);

// T309: the shell sub-fetch must forward the client's conditional-request
// headers, and a real 304 from the shell asset server must pass through to
// the client unchanged rather than being coerced into a 503.
//
// `new Request(url, request)` copies every header, including if-none-match
// and if-modified-since — validators that describe the LONG-TAIL url the
// client asked for, not the shell asset being read, but they are also the
// only validators a conforming client can present for the shell asset
// itself once the origin starts emitting an ETag/Last-Modified on it. The
// old behaviour (strip both headers unconditionally) meant the fallback
// could never honor a cache revalidation even when the origin supported
// one; production emits no ETag on these routes today (verified
// 2026-08-04), so this was latent rather than live.
describe.each(cases)(
  '%s/[[path]].js conditional requests',
  (_name, onRequest) => {
    function contextRecordingShellHeaders(seen, shellStatus = 200) {
      const request = new Request(
        'https://stellarindex.io/whatever/long-tail-id',
        {
          headers: {
            'if-none-match': '"some-shell-etag"',
            'if-modified-since': 'Wed, 21 Oct 2026 07:28:00 GMT',
          },
        },
      );
      return {
        request,
        env: {
          ASSETS: {
            fetch: async (req) => {
              const url = typeof req === 'string' ? req : req.url;
              if (url.includes('/shell/')) {
                seen.ifNoneMatch = req.headers.get('if-none-match');
                seen.ifModifiedSince = req.headers.get('if-modified-since');
                return new Response(
                  shellStatus === 304 ? null : '<html>shell</html>',
                  {
                    status: shellStatus,
                    headers: { 'content-type': 'text/html' },
                  },
                );
              }
              return new Response('not found', { status: 404 });
            },
          },
        },
      };
    }

    it('forwards validators to the shell sub-fetch', async () => {
      const seen = {};
      const res = await onRequest(contextRecordingShellHeaders(seen, 200));
      expect(seen.ifNoneMatch).toBe('"some-shell-etag"');
      expect(seen.ifModifiedSince).toBe('Wed, 21 Oct 2026 07:28:00 GMT');
      expect(res.status).toBe(200);
    });

    it('passes through a real 304 from the shell fetch instead of a 503', async () => {
      const seen = {};
      const res = await onRequest(contextRecordingShellHeaders(seen, 304));
      expect(res.status).toBe(304);
    });
  },
);

// Each handler must read ITS OWN shell. These files are copy-pasted from
// one another, and the pre-existing fake matched on `/shell/` alone — so a
// handler that fetched a sibling's shell passed every assertion.
describe('each handler fetches its own shell path', () => {
  it.each(cases)('%s', async (_name, onRequest, shellPath) => {
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
    expect(fetched).toBe(shellPath);
  });
});

// T291: the walk above only proves the handlers that EXIST behave; it
// cannot notice a dynamic route that has none. /sources/[name],
// /external/assets/[slug] and /embed/{asset,currency,pair}/[*] pre-render
// only the ids their API listing held at build time, and explorer deploys
// are manual, so every id that appeared since hard-404'd on the static
// host. Walk the app router instead: every dynamic route must either be
// covered by its family's shell-fallback handler (and bake the `shell`
// document that handler serves), or be listed with the reason its param
// set cannot grow between builds.
const appDir = path.resolve(functionsDir, '../src/app');

const CLOSED_AT_BUILD = new Map([
  ['blog/[slug]', 'repo markdown (loadBlogPosts)'],
  ['dexes/[source]', 'frontend DEX_INFO constant'],
  ['exchanges/[name]', 'frontend CEX_INFO constant'],
  ['protocols/[name]', 'frontend PROTOCOLS constant, dynamicParams = false'],
  ['research/adr/[id]', 'repo ADRs (loadADRs)'],
  ['research/architecture/[slug]', 'repo docs (loadArchitectureDocs)'],
  ['research/operations/[slug]', 'repo docs (loadOperationsDocs)'],
  ['status/incident/[slug]', 'repo incident files (loadIncidents)'],
]);

// API-derived and still uncovered: a live defect, not an exemption. The
// last test fails once its handler lands so the entry cannot outlive it.
const KNOWN_GAPS = new Map([
  [
    'convert/[from]/[to]',
    'two dynamic segments; the one-segment /<family>/shell/ contract does not fit',
  ],
]);

const familyOf = (segs) =>
  segs.slice(
    0,
    segs.findIndex((s) => s.startsWith('[')),
  );

function discoverDynamicRoutes(dir, relSegments = []) {
  const found = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    if (!entry.isDirectory()) continue;
    const segs = [...relSegments, entry.name];
    const pagePath = path.join(dir, entry.name, 'page.tsx');
    if (segs.some((s) => s.startsWith('[')) && fs.existsSync(pagePath)) {
      found.push({
        route: segs.join('/'),
        family: familyOf(segs),
        source: fs.readFileSync(pagePath, 'utf8'),
      });
    }
    found.push(...discoverDynamicRoutes(path.join(dir, entry.name), segs));
  }
  return found;
}

const dynamicRoutes = discoverDynamicRoutes(appDir);
if (dynamicRoutes.length === 0) {
  throw new Error(
    `discoverDynamicRoutes found no dynamic routes under ${appDir}`,
  );
}

const handlerFor = (family) =>
  path.join(functionsDir, ...family, '[[path]].js');

describe('every growable dynamic route has a shell fallback', () => {
  const growable = dynamicRoutes.filter(
    ({ route }) => !CLOSED_AT_BUILD.has(route) && !KNOWN_GAPS.has(route),
  );

  it.each(growable.map((r) => [r.route, r]))(
    '%s',
    (_route, { family, source }) => {
      const handler = handlerFor(family);
      expect(
        fs.existsSync(handler),
        `missing ${path.relative(functionsDir, handler)}`,
      ).toBe(true);
      expect(fs.readFileSync(handler, 'utf8')).toContain(
        `'/${family.join('/')}/shell/'`,
      );
      // That shell document exists only if the page bakes the sentinel
      // through generateStaticParams.
      expect(source).toMatch(/['"]shell['"]/);
    },
  );

  it('lists only routes that exist and still lack a handler', () => {
    const byRoute = new Map(dynamicRoutes.map((r) => [r.route, r]));
    for (const route of [...CLOSED_AT_BUILD.keys(), ...KNOWN_GAPS.keys()]) {
      expect(byRoute.has(route), `stale entry ${route}`).toBe(true);
    }
    for (const route of KNOWN_GAPS.keys()) {
      expect(
        fs.existsSync(handlerFor(byRoute.get(route).family)),
        `${route} now has a handler; drop it from KNOWN_GAPS`,
      ).toBe(false);
    }
  });
});
