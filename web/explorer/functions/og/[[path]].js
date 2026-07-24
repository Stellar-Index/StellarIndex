import { ImageResponse } from 'workers-og';

// Dynamic OG card generator (SEO plan D7). GET /og/{type}/{id} → a 1200×630 PNG
// (satori + resvg-wasm on CF Pages Functions), edge-cached. Market cards carry
// the LIVE price (near-real-time via the 60s edge cache + a tight upstream
// timeout). NB the live fetch hits our public API from the edge — fine at this
// volume + cache; revisit auth/rate-limits (R7) if it grows.

function code(s) {
  if (!s) return '';
  if (s === 'native') return 'XLM';
  const colon = s.indexOf(':');
  if (colon > 0) s = s.slice(colon + 1);
  const dash = s.indexOf('-');
  return (dash > 0 ? s.slice(0, dash) : s).toUpperCase();
}

function prettyLabel(type, id) {
  if (!id) return null;
  if (type === 'markets' && id.includes('~')) {
    const [b, q] = id.split('~');
    return `${code(b)} / ${code(q)}`;
  }
  if (type === 'assets') return code(id);
  return id.length > 24 ? `${id.slice(0, 10)}…${id.slice(-8)}` : id;
}

// SEC-08: a Map, not a plain object — a plain object keyed by an
// attacker-controlled `type` string lets `type === 'constructor'` (etc.)
// resolve an inherited Object.prototype member instead of missing cleanly.
export const TYPE_LABEL = new Map([
  ['markets', 'Market'],
  ['assets', 'Asset'],
  ['transactions', 'Transaction'],
  ['ledgers', 'Ledger'],
  ['accounts', 'Account'],
  ['contracts', 'Contract'],
  ['protocols', 'Protocol'],
  ['issuers', 'Issuer'],
]);

// input-validation: bound the id segment before it reaches decode, the
// upstream price fetch, or the satori render.
const MAX_ID_LENGTH = 160;

// SEC-15: liveSubline's upstream fetch is only worth making — and only
// safe to make unauthenticated, at the edge, on every request — when both
// legs of `base~quote` actually look like a canonical asset id the API
// accepts: `native`, `CODE-ISSUER` (up to a 12-char code + '-' + a 56-char
// G/C-strkey issuer), `pool:<hex>`, `fiat:<code>`, or a plain slug — never
// arbitrary attacker-controlled text (markup, whitespace, path traversal).
const ASSET_LEG_RE = /^[A-Za-z0-9_:-]{1,80}$/;

// Exported for unit tests only (functions/og/og.test.js) — CF Pages only
// invokes `onRequest`; these named exports have no runtime effect on it.
export async function liveSubline(type, rawId) {
  try {
    if (type === 'markets' && rawId.includes('~')) {
      const [base, quote] = rawId.split('~');
      if (!ASSET_LEG_RE.test(base) || !ASSET_LEG_RE.test(quote)) return null;
      const r = await fetch(
        `https://api.stellarindex.io/v1/price?asset=${encodeURIComponent(base)}&quote=${encodeURIComponent(quote)}`,
        { signal: AbortSignal.timeout(2500), headers: { 'user-agent': 'stellarindex-og/1' } },
      );
      if (r.ok) {
        const p = (await r.json())?.data?.price;
        if (p != null) {
          const n = Number(p);
          const fmt = n >= 1 ? n.toLocaleString('en-US', { maximumFractionDigits: 2 }) : n.toPrecision(4);
          return `1 ${code(base)} = ${fmt} ${code(quote)}`;
        }
      }
    }
  } catch { /* fall through to label-only card */ }
  return null;
}

export async function onRequest(context) {
  const { request, env } = context;

  // kill-switch: flip OG_DISABLED=1 as a CF Pages environment variable
  // (dashboard-only, no redeploy) to take this endpoint offline if it's
  // ever driving abusive load.
  if (env?.OG_DISABLED === '1') {
    return new Response('OG image generation is temporarily disabled.', { status: 503 });
  }

  const url = new URL(request.url);
  const parts = url.pathname.replace(/^\/og\/?/, '').split('/').filter(Boolean);
  const type = (parts[0] || 'home').replace(/[^a-z0-9-]/gi, '');

  // SEC-15: 404 unknown types before doing any work — 'home' is the only
  // pseudo-type without a TYPE_LABEL entry (the id-less site-wide card).
  if (type !== 'home' && !TYPE_LABEL.has(type)) {
    return new Response('Not found', { status: 404 });
  }

  let rawId = parts.slice(1).join('/') || '';
  // input-validation: reject oversized ids before decode/fetch/render.
  if (rawId.length > MAX_ID_LENGTH) {
    return new Response('Not found', { status: 404 });
  }
  // CS-009: decode the path segment AT MOST ONCE (the previous 2× loop
  // defeated the upstream ogImageFor encodeURIComponent, resurfacing raw
  // markup). Combined with esc() below this closes the SSRF/injection sink.
  try { rawId = decodeURIComponent(rawId); } catch { /* leave as-is */ }
  const label = prettyLabel(type, rawId) || 'Stellar pricing & protocol explorer';
  const kicker = TYPE_LABEL.has(type) ? `Stellar Index · ${TYPE_LABEL.get(type)}` : 'Stellar Index';
  const sub = await liveSubline(type, rawId);

  // CS-009: HTML-escape every interpolated value. Unescaped attacker input
  // reaching satori markup lets an injected `<img src=…>` trigger an
  // unauthenticated blind SSRF (satori fetches the src with no allow-list).
  const esc = (s) =>
    String(s == null ? '' : s).replace(/[&<>"']/g, (c) =>
      ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c],
    );

  const html = `
    <div style="display:flex;flex-direction:column;justify-content:space-between;width:1200px;height:630px;background:#0b0f1a;color:#ffffff;padding:84px;font-family:sans-serif;">
      <div style="display:flex;font-size:30px;color:#7aa2ff;font-weight:600;">${esc(kicker)}</div>
      <div style="display:flex;flex-direction:column;">
        <div style="display:flex;font-size:76px;font-weight:700;line-height:1.05;">${esc(label)}</div>
        ${sub ? `<div style="display:flex;font-size:40px;color:#cdd6e6;margin-top:18px;">${esc(sub)}</div>` : ''}
      </div>
      <div style="display:flex;font-size:26px;color:#8a93a6;">stellarindex.io</div>
    </div>`;

  return new ImageResponse(html, {
    width: 1200, height: 630,
    headers: { 'cache-control': 'public, s-maxage=60, stale-while-revalidate=300' },
  });
}
