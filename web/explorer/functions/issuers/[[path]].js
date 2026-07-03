// CF Pages Function for /issuers/* — serves the pre-rendered top-100
// pages first, else the client shell (site audit S-022: issuers beyond
// the top-100 hard-404'd while search + asset pages linked to them).
// See functions/transactions/[[path]].js for the full rationale.
export async function onRequest(context) {
  const { request, env } = context;
  const url = new URL(request.url);

  const asset = await env.ASSETS.fetch(request);
  if (asset.status !== 404) {
    return asset;
  }

  const shell = await env.ASSETS.fetch(
    new Request(new URL('/issuers/shell/', url.origin), request),
  );
  return new Response(shell.body, { status: 200, headers: shell.headers });
}
