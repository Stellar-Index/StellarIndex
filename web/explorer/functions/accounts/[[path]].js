// CF Pages Function for /accounts/* — serves real assets first (the /accounts/
// richlist, or future pre-rendered curated accounts), else the shell. See
// functions/transactions/[[path]].js for the full rationale (SEO plan D1).
export async function onRequest(context) {
  const { request, env } = context;
  const url = new URL(request.url);

  const asset = await env.ASSETS.fetch(request);
  if (asset.status !== 404) {
    return asset;
  }

  const shell = await env.ASSETS.fetch(
    new Request(new URL('/accounts/shell/', url.origin), request),
  );
  // REL-02: propagate the shell fetch's real status. Forcing 200
  // unconditionally turned a missing/broken shell into a soft-200 error
  // page — indistinguishable from a real one to caches, monitors, and bots.
  return new Response(shell.body, { status: shell.ok ? 200 : 503, headers: shell.headers });
}
