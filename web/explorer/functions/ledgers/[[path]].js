// CF Pages Function for /ledgers/* — serves real assets first (the /ledgers/
// list, or future pre-rendered recent ledgers), else the shell. See
// functions/transactions/[[path]].js for the full rationale (SEO plan D1;
// Spike A proved a _redirects catch-all clobbers static files).
export async function onRequest(context) {
  const { request, env } = context;
  const url = new URL(request.url);

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
  //
  // Latent rather than live today: production emits no ETag or
  // Last-Modified on these routes (verified 2026-08-04), so a client has
  // no validator to send. That is a property of the platform's current
  // behaviour, not something this handler guarantees — so make the
  // sub-fetch unconditional rather than depending on it (cold audit
  // 2026-08-04).
  const shellHeaders = new Headers(request.headers);
  shellHeaders.delete('if-none-match');
  shellHeaders.delete('if-modified-since');

  const shell = await env.ASSETS.fetch(
    new Request(new URL('/ledgers/shell/', url.origin), {
      method: request.method,
      headers: shellHeaders,
      redirect: request.redirect,
    }),
  );
  // REL-02: propagate the shell fetch's real status. Forcing 200
  // unconditionally turned a missing/broken shell into a soft-200 error
  // page — indistinguishable from a real one to caches, monitors, and bots.
  return new Response(shell.body, { status: shell.ok ? 200 : 503, headers: shell.headers });
}
