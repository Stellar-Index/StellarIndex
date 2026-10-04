// CF Pages Function for /icon?u=<https image url> — same-origin proxy for
// issuer-hosted SEP-1 asset icons, so visitors' browsers never contact the
// issuer's host (CSP img-src is 'self' data:). The edge resolves and fetches
// the URL itself, so DNS rebinding toward a viewer's LAN is not reachable.
const MAX_BYTES = 1024 * 1024;
const MAX_REDIRECTS = 3;
const MAX_URL_LEN = 2048;
const FETCH_TIMEOUT_MS = 5000;
const IMAGE_TYPES = new Set([
  'image/png',
  'image/jpeg',
  'image/gif',
  'image/webp',
  'image/avif',
  'image/bmp',
  'image/x-icon',
  'image/vnd.microsoft.icon',
  'image/jpg',
  'image/svg+xml',
]);

// The edge cannot reach private ranges anyway; refusing IP literals and
// internal-looking names keeps the proxy to public DNS hostnames only.
export function isProxyableUrl(raw) {
  if (typeof raw !== 'string' || raw.length === 0 || raw.length > MAX_URL_LEN) {
    return false;
  }
  let u;
  try {
    u = new URL(raw);
  } catch {
    return false;
  }
  if (u.protocol !== 'https:' || u.username || u.password) return false;
  if (u.port && u.port !== '443') return false;
  const host = u.hostname.toLowerCase().replace(/\.$/, '');
  if (!host.includes('.') || host.includes(':')) return false;
  if (/^[0-9.]+$/.test(host)) return false;
  if (/^0x[0-9a-f]+$/i.test(host.split('.').pop())) return false;
  if (/\.(local|localhost|internal|lan|home|corp)$/.test(host)) return false;
  return true;
}

function fail(status) {
  return new Response(null, {
    status,
    headers: {
      'Cache-Control': 'public, max-age=300',
      'X-Content-Type-Options': 'nosniff',
    },
  });
}

async function fetchIcon(start) {
  let target = start;
  for (let hop = 0; hop <= MAX_REDIRECTS; hop++) {
    const res = await fetch(target, {
      redirect: 'manual',
      headers: { Accept: 'image/*' },
      signal: AbortSignal.timeout(FETCH_TIMEOUT_MS),
    });
    if (res.status < 300 || res.status >= 400) return res;
    const loc = res.headers.get('location');
    if (!loc) return null;
    let next;
    try {
      next = new URL(loc, target).toString();
    } catch {
      return null;
    }
    if (!isProxyableUrl(next)) return null;
    target = next;
  }
  return null;
}

async function readCapped(res) {
  const declared = Number(res.headers.get('content-length'));
  if (declared > MAX_BYTES || !res.body) return null;
  const reader = res.body.getReader();
  const chunks = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.byteLength;
    if (total > MAX_BYTES) {
      await reader.cancel();
      return null;
    }
    chunks.push(value);
  }
  return new Blob(chunks);
}

export async function onRequest(context) {
  const { request } = context;
  if (request.method !== 'GET' && request.method !== 'HEAD') return fail(405);
  const target = new URL(request.url).searchParams.get('u');
  if (!isProxyableUrl(target)) return fail(400);

  let res;
  try {
    res = await fetchIcon(target);
  } catch {
    return fail(502);
  }
  if (!res || !res.ok) return fail(404);

  const type = (res.headers.get('content-type') || '')
    .split(';')[0]
    .trim()
    .toLowerCase();
  if (!IMAGE_TYPES.has(type)) return fail(415);
  const outType = type === 'image/jpg' ? 'image/jpeg' : type;

  const body = await readCapped(res).catch(() => null);
  if (!body) return fail(413);

  return new Response(request.method === 'HEAD' ? null : body, {
    headers: {
      'Content-Type': outType,
      'Cache-Control': 'public, max-age=86400',
      'X-Content-Type-Options': 'nosniff',
      // An SVG opened as a top-level document must not run script here.
      'Content-Security-Policy': "default-src 'none'; sandbox",
    },
  });
}
