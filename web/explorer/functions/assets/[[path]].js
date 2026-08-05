// CF Pages Function for /assets/* — serves the pre-rendered pages first,
// else the client shell.
//
// 2026-08-05: /assets/[slug] pre-renders the top-500 listing + verified
// catalogue at build time. Migration 0134 gave EVERY classic asset
// (194k) a unique public slug, the API resolves them
// (/v1/assets/{slug}), and the listing links by slug — so any asset
// outside the pre-render snapshot hard-404'd on the static host (live
// report: /assets/usdt-gasu4kif). Same shell-fallback pattern as
// /markets, /accounts, /contracts, /issuers, /ledgers, /transactions
// (S-022 / S1b): pre-rendered slugs keep their SEO, everything else
// hydrates from the API via the /assets/shell/ client view.
export async function onRequest(context) {
  const { request, env } = context;
  const url = new URL(request.url);

  const asset = await env.ASSETS.fetch(request);
  if (asset.status !== 404) {
    return asset;
  }

  // Strip the client's conditional-request headers from the shell
  // sub-fetch — they describe the long-tail URL, not the shell asset
  // (see functions/markets/[[path]].js for the full rationale; cold
  // audit 2026-08-04).
  const shellHeaders = new Headers(request.headers);
  shellHeaders.delete('if-none-match');
  shellHeaders.delete('if-modified-since');

  const shell = await env.ASSETS.fetch(
    new Request(new URL('/assets/shell/', url.origin), {
      method: request.method,
      headers: shellHeaders,
      redirect: request.redirect,
    }),
  );
  // REL-02: propagate the shell fetch's real status — forcing 200 turns
  // a missing/broken shell into a soft-200 error page.
  return new Response(shell.body, { status: shell.ok ? 200 : 503, headers: shell.headers });
}
