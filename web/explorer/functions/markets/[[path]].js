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

  const shell = await env.ASSETS.fetch(
    new Request(new URL('/markets/shell/', url.origin), request),
  );
  return new Response(shell.body, { status: 200, headers: shell.headers });
}
