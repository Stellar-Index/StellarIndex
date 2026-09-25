// CF Pages Function — POST /client-errors
//
// Beacon target for src/components/RouteError.tsx: a route-segment throw
// otherwise only reaches the reporting user's own browser console, which
// nobody else ever reads in production. This lands the record in
// Cloudflare's per-request Workers logs instead — no external vendor, no
// new secret. Best-effort: a malformed body degrades to a bare 204 rather
// than failing the beacon back to the client; an oversized one is 413'd
// without being buffered.
const MAX_BODY_BYTES = 4096;
const MAX_FIELD_LENGTH = 500;

// A declared length is only a fast reject — the header can lie or be absent
// (chunked), so the body read itself is also capped. Returns null over cap.
async function readCappedText(request, maxBytes) {
  const declared = Number(request.headers.get('content-length'));
  if (declared > maxBytes) return null;
  if (!request.body) return '';

  const reader = request.body.getReader();
  const chunks = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.byteLength;
    if (total > maxBytes) {
      await reader.cancel();
      return null;
    }
    chunks.push(value);
  }
  const bytes = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return new TextDecoder().decode(bytes);
}

function truncate(value) {
  return typeof value === 'string'
    ? value.slice(0, MAX_FIELD_LENGTH)
    : undefined;
}

export async function onRequestPost(context) {
  const { request } = context;

  const text = await readCappedText(request, MAX_BODY_BYTES);
  if (text === null) {
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
