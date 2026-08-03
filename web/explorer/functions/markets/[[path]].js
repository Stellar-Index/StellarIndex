// CF Pages Function for /markets/* — serves the pre-rendered pages first,
// else the client shell.
//
// Site-audit S1/S1b/S7: /markets/[pair] pre-renders the top 500 pairs by
// 24h USD volume at BUILD time, and anything outside that snapshot
// hard-404'd. The set was not too small — it was stale. The 404ing pairs
// were comfortably inside the limit in live data (USDCAllow rank 27 at
// $6.4M, GoogleLiquid rank 51, HBAR rank 100); markets simply churn
// between builds, so a pair that enters the ranking after the last deploy
// dead-ends until the next one.
//
// Consequences that were live in production:
//   - the /markets listing linked to its OWN dead pages — 5 of the 55
//     Stellar rows it displayed, including ROW 1
//   - the /network "Top Stellar markets" widget ranks a DIFFERENT
//     population (/v1/pools, on-chain only) than the pre-render list
//     (/v1/markets, CEX-dominated), so 2 of its 8 links 404'd and which
//     ones moved with volume ranking
//
// Raising the pre-render limit does not fix a staleness bug. This is the
// same fallback already used by accounts / contracts / issuers / ledgers /
// transactions (S-022), and it decouples correctness from build freshness
// entirely: pre-rendered pairs keep their SEO, everything else hydrates
// from the API like any other dynamic route.
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
    new Request(new URL('/markets/shell/', url.origin), {
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
