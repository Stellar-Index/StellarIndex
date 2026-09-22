// CF Pages Function — POST /client-errors
//
// Beacon target for src/components/RouteError.tsx: a route-segment throw
// otherwise only reaches the reporting user's own browser console, which
// nobody else ever reads in production. This lands the record in
// Cloudflare's per-request Workers logs instead — no external vendor, no
// new secret. Best-effort: a malformed or oversized body degrades to a
// bare 204 rather than failing the beacon back to the client.
const MAX_BODY_BYTES = 4096;
const MAX_FIELD_LENGTH = 500;

function truncate(value) {
  return typeof value === 'string'
    ? value.slice(0, MAX_FIELD_LENGTH)
    : undefined;
}

export async function onRequestPost(context) {
  const { request } = context;

  const text = await request.text();
  if (text.length > MAX_BODY_BYTES) {
    return new Response(null, { status: 413 });
  }

  let body;
  try {
    body = JSON.parse(text);
  } catch {
    return new Response(null, { status: 204 });
  }

  console.error('[client-error]', {
    message: truncate(body?.message),
    digest: truncate(body?.digest),
    section: truncate(body?.section),
    path: truncate(body?.path),
  });

  return new Response(null, { status: 204 });
}

export async function onRequestGet() {
  return new Response(null, { status: 405 });
}
