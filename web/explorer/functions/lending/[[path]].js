// CF Pages Function for /lending/* — serves the pre-rendered pages first,
// else the client shell.
//
// T278: web/explorer/src/app/lending/[pool] pre-renders every pool
// /v1/lending/pools currently lists plus the curated Blend
// factory/backstop contracts at BUILD time — but this directory did not
// exist, so a pool the Blend factory spawns between builds hard-404'd on
// the static host with no fallback. Same shell-fallback pattern already
// used by /assets, /markets, /accounts, /contracts, /issuers, /ledgers,
// /transactions (S-022 / S1b): pre-rendered pools keep their SEO,
// everything else hydrates from the API via LendingPoolPathView.
export async function onRequest(context) {
  const { request, env } = context;
  const url = new URL(request.url);

  const asset = await env.ASSETS.fetch(request);
  if (asset.status !== 404) {
    return asset;
  }

  // Strip the client's conditional-request headers from the shell
  // sub-fetch — they describe the long-tail URL, not the shell asset
  // (see functions/markets/[[path]].js for the full rationale).
  const shellHeaders = new Headers(request.headers);
  shellHeaders.delete('if-none-match');
  shellHeaders.delete('if-modified-since');

  const shell = await env.ASSETS.fetch(
    new Request(new URL('/lending/shell/', url.origin), {
      method: request.method,
      headers: shellHeaders,
      redirect: request.redirect,
    }),
  );
  // REL-02: propagate the shell fetch's real status — forcing 200 turns
  // a missing/broken shell into a soft-200 error page.
  return new Response(shell.body, {
    status: shell.ok ? 200 : 503,
    headers: shell.headers,
  });
}
