// Shared implementation for the CF Pages shell-fallback Functions
// (accounts, assets, contracts, issuers, ledgers, lending, markets,
// transactions, insights/sponsors, insights/creators — S-022/S1b; sources,
// external/assets, embed/{asset,currency,pair} — T291). Each
// route serves a real pre-rendered asset first, else the route's static
// shell. A leading underscore excludes this directory from CF Pages
// routing, so it is never itself served as a route.
//
// T309: forward the client's conditional-request headers to the shell
// sub-fetch and pass through a real 304 (body-less) instead of stripping
// if-none-match/if-modified-since. Production emits no ETag/Last-Modified
// on these routes today (verified 2026-08-04), so this was latent — but
// stripping the validators meant the fallback could never honor one once
// the origin started sending them.
//
// GH-916: `public/_headers` is applied by the Pages static-asset server
// only. Every route below is caught by this Function's `[[path]].js`
// wildcard, so NONE of its responses — real pre-rendered asset or shell
// fallback — ever reach that asset server, and none carried CSP/HSTS/
// X-Frame-Options/nosniff/Referrer-Policy/Permissions-Policy. Mirror the
// two rule blocks `public/_headers` declares (`/*` and `/embed/*`) here and
// apply the matching one to every response this handler returns.
// shell-fallback.test.js parses `public/_headers` itself and asserts these
// constants match it, so the two cannot drift apart silently.
const SECURITY_HEADERS = {
  default: {
    'X-Content-Type-Options': 'nosniff',
    'X-Frame-Options': 'DENY',
    'Referrer-Policy': 'strict-origin-when-cross-origin',
    'Permissions-Policy':
      'accelerometer=(), camera=(), geolocation=(), microphone=(), payment=(), usb=()',
    'Strict-Transport-Security': 'max-age=31536000; includeSubDomains',
    'Content-Security-Policy':
      "default-src 'self'; connect-src 'self' https://api.stellarindex.io https://api.testnet.stellarindex.io https://api.futurenet.stellarindex.io; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; object-src 'none'; base-uri 'self'; form-action 'self'",
  },
  // /embed/* is designed to be iframed by customer sites (see
  // public/_headers) — ALLOWALL + `frame-ancestors *` instead of the
  // default block's DENY, mirroring the static rule exactly.
  embed: {
    'X-Content-Type-Options': 'nosniff',
    'X-Frame-Options': 'ALLOWALL',
    'Referrer-Policy': 'strict-origin-when-cross-origin',
    'Permissions-Policy':
      'accelerometer=(), camera=(), geolocation=(), microphone=(), payment=(), usb=()',
    'Strict-Transport-Security': 'max-age=31536000; includeSubDomains',
    'Content-Security-Policy':
      "default-src 'self'; connect-src 'self' https://api.stellarindex.io https://api.testnet.stellarindex.io https://api.futurenet.stellarindex.io; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; object-src 'none'; base-uri 'self'; frame-ancestors *; form-action 'self'",
  },
};

function headersVariantFor(shellPath) {
  return shellPath.startsWith('/embed/') ? 'embed' : 'default';
}

function withSecurityHeaders(response, variant) {
  const headers = new Headers(response.headers);
  for (const [name, value] of Object.entries(SECURITY_HEADERS[variant])) {
    headers.set(name, value);
  }
  return new Response(response.body, {
    status: response.status,
    statusText: response.statusText,
    headers,
  });
}

// Real HTTP 301 carrying the same security headers as the shell responses.
export function permanentRedirect(location) {
  return withSecurityHeaders(
    new Response(null, { status: 301, headers: { Location: location } }),
    'default',
  );
}

export async function shellFallback(context, shellPath) {
  const { request, env } = context;
  const url = new URL(request.url);
  const variant = headersVariantFor(shellPath);
  try {
    const asset = await env.ASSETS.fetch(request);
    if (asset.status !== 404) {
      return withSecurityHeaders(asset, variant);
    }

    const shell = await env.ASSETS.fetch(
      new Request(new URL(shellPath, url.origin), {
        method: request.method,
        headers: request.headers,
        redirect: request.redirect,
      }),
    );

    // A conforming asset server answers 304 with no body when the shell's
    // own validator matches the client's — pass that through unchanged.
    if (shell.status === 304) {
      return withSecurityHeaders(
        new Response(null, { status: 304, headers: shell.headers }),
        variant,
      );
    }
    // REL-02: propagate the shell fetch's real status otherwise. Forcing
    // 200 unconditionally turned a missing/broken shell into a soft-200
    // error page — indistinguishable from a real one to caches, monitors,
    // and bots.
    return withSecurityHeaders(
      new Response(shell.body, {
        status: shell.ok ? 200 : 503,
        headers: shell.headers,
      }),
      variant,
    );
  } catch {
    // env.ASSETS.fetch (and the Request/URL construction around it) can
    // throw on a worker-runtime fault (binding unavailable, network fault)
    // instead of resolving to a Response. Unhandled, that throw surfaces as
    // CF's raw, unbranded 500 error page rather than the 503 this handler
    // already returns for a failed shell fetch — treat both failure modes
    // the same way.
    return withSecurityHeaders(
      new Response('Service temporarily unavailable', { status: 503 }),
      variant,
    );
  }
}
