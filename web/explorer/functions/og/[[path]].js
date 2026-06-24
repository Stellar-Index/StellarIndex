import { ImageResponse } from 'workers-og';

// Dynamic OG card generator (SEO plan D7). GET /og/{type}/{id} → a 1200×630 PNG
// rendered with satori + resvg-wasm, edge-cached. v1 is a branded entity card;
// live-data + richer layout is the next iteration. All og:image meta will point
// here once verified.
export async function onRequest(context) {
  const { request } = context;
  const url = new URL(request.url);
  const parts = url.pathname.replace(/^\/og\/?/, '').split('/').filter(Boolean);
  const type = (parts[0] || 'home').replace(/[^a-z0-9-]/gi, '');
  const rawId = decodeURIComponent(parts.slice(1).join('/') || '');
  const id = rawId.length > 22 ? rawId.slice(0, 10) + '…' + rawId.slice(-8) : rawId;
  const label = id ? `${type} ${id}` : 'Stellar pricing & protocol explorer';

  const html = `
    <div style="display:flex;flex-direction:column;justify-content:space-between;width:1200px;height:630px;background:#0b0f1a;color:#ffffff;padding:80px;font-family:sans-serif;">
      <div style="display:flex;font-size:30px;color:#7aa2ff;font-weight:600;">Stellar Index</div>
      <div style="display:flex;font-size:68px;font-weight:700;line-height:1.1;">${label}</div>
      <div style="display:flex;font-size:26px;color:#8a93a6;">stellarindex.io</div>
    </div>`;

  return new ImageResponse(html, {
    width: 1200,
    height: 630,
    headers: { 'cache-control': 'public, s-maxage=60, stale-while-revalidate=300' },
  });
}
