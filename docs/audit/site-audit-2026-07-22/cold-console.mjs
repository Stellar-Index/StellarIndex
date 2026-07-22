// Capture console + exceptions from the very first byte of a cold page load.
// Connects CDP and enables Log/Runtime BEFORE issuing Page.navigate, which is
// the thing the MCP browser bridge cannot do (it can only inject post-hydration).
const [, , targetUrl, widthArg] = process.argv;
const width = Number(widthArg || 1440);

const list = await (await fetch('http://127.0.0.1:9222/json/list')).json();
let page = list.find((t) => t.type === 'page');
if (!page) {
  page = await (await fetch('http://127.0.0.1:9222/json/new?about:blank')).json();
}

const ws = new WebSocket(page.webSocketDebuggerUrl);
let id = 0;
const pending = new Map();
const events = { consoleErrors: [], consoleWarnings: [], exceptions: [], failedRequests: [] };

const send = (method, params = {}) =>
  new Promise((res) => {
    const msgId = ++id;
    pending.set(msgId, res);
    ws.send(JSON.stringify({ id: msgId, method, params }));
  });

ws.addEventListener('message', (m) => {
  const msg = JSON.parse(m.data);
  if (msg.id && pending.has(msg.id)) { pending.get(msg.id)(msg.result); pending.delete(msg.id); return; }
  const p = msg.params || {};
  switch (msg.method) {
    case 'Log.entryAdded':
      if (p.entry.level === 'error') events.consoleErrors.push(`[${p.entry.source}] ${String(p.entry.text).slice(0, 170)}`);
      else if (p.entry.level === 'warning') events.consoleWarnings.push(String(p.entry.text).slice(0, 140));
      break;
    case 'Runtime.consoleAPICalled':
      if (p.type === 'error') events.consoleErrors.push('console.error: ' + p.args.map((a) => String(a.value ?? a.description ?? '')).join(' ').slice(0, 170));
      else if (p.type === 'warning') events.consoleWarnings.push('console.warn: ' + p.args.map((a) => String(a.value ?? a.description ?? '')).join(' ').slice(0, 140));
      break;
    case 'Runtime.exceptionThrown':
      events.exceptions.push(String(p.exceptionDetails.exception?.description || p.exceptionDetails.text).slice(0, 200));
      break;
    case 'Network.loadingFailed':
      events.failedRequests.push(`${p.type} ${String(p.errorText).slice(0, 60)}`);
      break;
    case 'Network.responseReceived':
      if (p.response.status >= 400) events.failedRequests.push(`${p.response.status} ${String(p.response.url).split('stellarindex.io')[1] || p.response.url}`);
      break;
  }
});

await new Promise((r) => ws.addEventListener('open', r));

// Enable BEFORE navigating — this is the whole point.
await send('Log.enable');
await send('Runtime.enable');
await send('Network.enable');
await send('Page.enable');
await send('Emulation.setDeviceMetricsOverride', { width, height: 900, deviceScaleFactor: 1, mobile: width < 500 });

await send('Page.navigate', { url: targetUrl });
await new Promise((r) => setTimeout(r, 16000));

console.log(JSON.stringify({
  url: targetUrl,
  viewport: width,
  consoleErrors: [...new Set(events.consoleErrors)].slice(0, 10),
  consoleWarnings: [...new Set(events.consoleWarnings)].slice(0, 6),
  exceptions: [...new Set(events.exceptions)].slice(0, 6),
  failedRequests: [...new Set(events.failedRequests)].slice(0, 10),
}, null, 1));
ws.close();
