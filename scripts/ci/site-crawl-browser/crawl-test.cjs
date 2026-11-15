// Self-test for crawl.cjs: a fixture site whose every page fetches one URL (or none), against an
// API that answers with CORS, answers without it, or is not listening, and against a page that
// calls an origin other than the one the crawl was given. Run: node crawl-test.cjs
const http = require('http');
const path = require('path');
const { execFile } = require('child_process');

const listen = (handler) => new Promise((resolve) => {
  const s = http.createServer(handler);
  s.listen(0, '127.0.0.1', () => resolve(s));
});
const origin = (s) => `http://127.0.0.1:${s.address().port}`;

(async () => {
  let fetchUrl = '';
  const site = await listen((req, res) => {
    res.setHeader('content-type', 'text/html');
    const script = fetchUrl ? `<script>fetch(${JSON.stringify(fetchUrl)}).catch(() => {})</script>` : '';
    res.end(`<title>t</title><main><h1>ok</h1></main>${script}`);
  });
  const withCors = await listen((req, res) => {
    res.setHeader('access-control-allow-origin', '*');
    res.end('{}');
  });
  const noCors = await listen((req, res) => res.end('{}'));
  const closed = await listen(() => {});
  const closedOrigin = origin(closed);
  await new Promise((r) => closed.close(r));

  const noAnswer = /FAIL: .*no answer \/v1\/x/;
  const wrong = /FAIL: .*calls the wrong API origin/;
  // [name, <api> argument, URL each page fetches, exit code, required output]
  const cases = [
    ['an API that answers with CORS is a clean crawl', origin(withCors), origin(withCors) + '/v1/x', 0, null],
    ['an API answer the page may not read is a finding', origin(noCors), origin(noCors) + '/v1/x', 1, noAnswer],
    ['an API that refuses the connection is a finding', closedOrigin, closedOrigin + '/v1/x', 1, noAnswer],
    ['a page calling another origin\'s /v1/ is a finding', origin(noCors), origin(withCors) + '/v1/x', 1, wrong],
    ['a page calling /v1/ on the site origin is a finding', origin(withCors), '/v1/x', 1, wrong],
    ['a crawl in which no page calls the API is a finding', origin(withCors), '', 1, /FAIL: no page called the API/],
  ];
  let bad = 0;
  for (const [name, a, f, want, must] of cases) {
    fetchUrl = f;
    // Not spawnSync: the fixture servers answer from this process's event loop.
    const r = await new Promise((resolve) => {
      execFile(process.execPath, [path.join(__dirname, 'crawl.cjs'), origin(site), a, '2'],
        { env: { ...process.env, CRAWL_SETTLE_MS: '500' } },
        (err, stdout, stderr) => resolve({ status: err ? err.code : 0, out: stdout + stderr }));
    });
    const good = r.status === want && (!must || must.test(r.out));
    console.log(`  ${good ? 'ok  ' : 'FAIL'} ${name} (exit ${r.status})`);
    if (!good) { bad++; console.log(r.out); }
  }
  site.close(); withCors.close(); noCors.close();
  process.exit(bad ? 1 : 0);
})();
