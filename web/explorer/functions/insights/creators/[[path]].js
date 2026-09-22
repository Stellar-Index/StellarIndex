// CF Pages Function for /insights/creators/* — serves real assets first (the
// /insights/creators/ board itself), else the one built per-address shell.
// See functions/transactions/[[path]].js for the full rationale (SEO plan D1).
//
// The per-address route is a single `shell` document because no build can
// enumerate the address set behind it; /creator/{g} 301s here via
// public/_redirects, so this function is what makes that alias resolve
// rather than 404 on the long tail.
export async function onRequest(context) {
  const { request, env } = context;
  const url = new URL(request.url);
  try {
    const asset = await env.ASSETS.fetch(request);
    if (asset.status !== 404) {
      return asset;
    }

    // Strip the client's conditional-request headers from the shell
    // sub-fetch. `new Request(url, request)` copies every header, including
    // if-none-match / if-modified-since — validators that describe the
    // LONG-TAIL url the client asked for, not the shell asset we are about
    // to read. If one ever matched the shell's own validator the asset
    // server would answer 304 with a null body, `shell.ok` would be false
    // (304 is outside 200-299), and this handler would turn a healthy cache
    // revalidation into a 503 with an empty body — on the second and every
    // later visit to that url, including every search-engine recrawl.
    const shellHeaders = new Headers(request.headers);
    shellHeaders.delete('if-none-match');
    shellHeaders.delete('if-modified-since');

    const shell = await env.ASSETS.fetch(
      new Request(new URL('/insights/creators/shell/', url.origin), {
        method: request.method,
        headers: shellHeaders,
        redirect: request.redirect,
      }),
    );
    // REL-02: propagate the shell fetch's real status. Forcing 200
    // unconditionally turned a missing/broken shell into a soft-200 error
    // page — indistinguishable from a real one to caches, monitors, and bots.
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
