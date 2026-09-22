// Shared implementation for the CF Pages shell-fallback Functions
// (accounts, assets, contracts, issuers, ledgers, lending, markets,
// transactions, insights/sponsors, insights/creators — S-022/S1b). Each
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
export async function shellFallback(context, shellPath) {
  const { request, env } = context;
  const url = new URL(request.url);
  try {
    const asset = await env.ASSETS.fetch(request);
    if (asset.status !== 404) {
      return asset;
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
      return new Response(null, { status: 304, headers: shell.headers });
    }
    // REL-02: propagate the shell fetch's real status otherwise. Forcing
    // 200 unconditionally turned a missing/broken shell into a soft-200
    // error page — indistinguishable from a real one to caches, monitors,
    // and bots.
    return new Response(shell.body, {
      status: shell.ok ? 200 : 503,
      headers: shell.headers,
    });
  } catch {
    // env.ASSETS.fetch (and the Request/URL construction around it) can
    // throw on a worker-runtime fault (binding unavailable, network fault)
    // instead of resolving to a Response. Unhandled, that throw surfaces as
    // CF's raw, unbranded 500 error page rather than the 503 this handler
    // already returns for a failed shell fetch — treat both failure modes
    // the same way.
    return new Response('Service temporarily unavailable', { status: 503 });
  }
}
