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

const TYPE_LABEL = {
  markets: 'Market', assets: 'Asset', transactions: 'Transaction',
  ledgers: 'Ledger', accounts: 'Account', contracts: 'Contract', protocols: 'Protocol',
};

async function liveSubline(type, rawId) {
  try {
    if (type === 'markets' && rawId.includes('~')) {
      const [base, quote] = rawId.split('~');
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
  const { request } = context;
  const url = new URL(request.url);
  const parts = url.pathname.replace(/^\/og\/?/, '').split('/').filter(Boolean);
  const type = (parts[0] || 'home').replace(/[^a-z0-9-]/gi, '');
  let rawId = parts.slice(1).join('/') || '';
  for (let i = 0; i < 2; i++) {
    try { const d = decodeURIComponent(rawId); if (d === rawId) break; rawId = d; } catch { break; }
  }
  const label = prettyLabel(type, rawId) || 'Stellar pricing & protocol explorer';
  const kicker = TYPE_LABEL[type] ? `Stellar Index · ${TYPE_LABEL[type]}` : 'Stellar Index';
  const sub = await liveSubline(type, rawId);

  const html = `
    <div style="display:flex;flex-direction:column;justify-content:space-between;width:1200px;height:630px;background:#0b0f1a;color:#ffffff;padding:84px;font-family:sans-serif;">
      <div style="display:flex;font-size:30px;color:#7aa2ff;font-weight:600;">${kicker}</div>
      <div style="display:flex;flex-direction:column;">
        <div style="display:flex;font-size:76px;font-weight:700;line-height:1.05;">${label}</div>
        ${sub ? `<div style="display:flex;font-size:40px;color:#cdd6e6;margin-top:18px;">${sub}</div>` : ''}
      </div>
      <div style="display:flex;font-size:26px;color:#8a93a6;">stellarindex.io</div>
    </div>`;

  return new ImageResponse(html, {
    width: 1200, height: 630,
    headers: { 'cache-control': 'public, s-maxage=60, stale-while-revalidate=300' },
  });
}
