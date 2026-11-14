// Self-test for crawl.cjs: a fixture site whose every page fetches one API URL, against an API
// that answers with CORS, answers without it, or is not listening. Run: node crawl-test.cjs
const http = require('http');
const path = require('path');
const { execFile } = require('child_process');

const listen = (handler) => new Promise((resolve) => {
  const s = http.createServer(handler);
  s.listen(0, '127.0.0.1', () => resolve(s));
});
const origin = (s) => `http://127.0.0.1:${s.address().port}`;

(async () => {
  let api = '';
  const site = await listen((req, res) => {
    res.setHeader('content-type', 'text/html');
    res.end(`<title>t</title><main><h1>ok</h1></main><script>fetch(${JSON.stringify(api + '/v1/x')}).catch(() => {})</script>`);
  });
  const withCors = await listen((req, res) => {
    res.setHeader('access-control-allow-origin', '*');
    res.end('{}');
  });
  const noCors = await listen((req, res) => res.end('{}'));
  const closed = await listen(() => {});
  const closedOrigin = origin(closed);
  await new Promise((r) => closed.close(r));

  const cases = [
    ['an API that answers with CORS is a clean crawl', origin(withCors), 0],
    ['an API answer the page may not read is a finding', origin(noCors), 1],
    ['an API that refuses the connection is a finding', closedOrigin, 1],
  ];
  let bad = 0;
  for (const [name, a, want] of cases) {
    api = a;
    // Not spawnSync: the fixture servers answer from this process's event loop.
    const r = await new Promise((resolve) => {
      execFile(process.execPath, [path.join(__dirname, 'crawl.cjs'), origin(site), a, '2'],
        { env: { ...process.env, CRAWL_SETTLE_MS: '500' } },
        (err, stdout, stderr) => resolve({ status: err ? err.code : 0, out: stdout + stderr }));
    });
    const good = r.status === want && (want === 0 || /FAIL: .*no answer \/v1\/x/.test(r.out));
    console.log(`  ${good ? 'ok  ' : 'FAIL'} ${name} (exit ${r.status})`);
    if (!good) { bad++; console.log(r.out); }
  }
  site.close(); withCors.close(); noCors.close();
  process.exit(bad ? 1 : 0);
})();
