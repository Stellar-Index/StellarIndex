import { spawnSync } from 'node:child_process';
import { cpSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

// The script the build runs over out/: public/_redirects and public/llms.txt
// name the mainnet API and must not ship that way into another network.
const SCRIPT = join(process.cwd(), '../../scripts/ci/explorer-network-hosts.sh');
const MAINNET = 'https://api.stellarindex.io';

let dir = '';
afterEach(() => {
  if (dir) rmSync(dir, { recursive: true, force: true });
});

function run(apiBase?: string) {
  dir = mkdtempSync(join(tmpdir(), 'hosts-'));
  for (const f of ['_redirects', 'llms.txt']) {
    cpSync(join(process.cwd(), 'public', f), join(dir, f));
  }
  const env = { ...process.env };
  delete env.NEXT_PUBLIC_API_BASE_URL;
  if (apiBase) env.NEXT_PUBLIC_API_BASE_URL = apiBase;
  const res = spawnSync('bash', [SCRIPT, dir], { env, encoding: 'utf8' });
  return {
    status: res.status,
    redirects: readFileSync(join(dir, '_redirects'), 'utf8'),
    llms: readFileSync(join(dir, 'llms.txt'), 'utf8'),
  };
}

describe('explorer-network-hosts.sh', () => {
  it('points a testnet build at the testnet API', () => {
    const api = 'https://api.testnet.stellarindex.io';
    const out = run(api);
    expect(out.status).toBe(0);
    expect(out.redirects).toContain(`/v1/*     ${api}/v1/:splat 301`);
    expect(out.redirects).not.toContain(MAINNET);
    expect(out.llms).toContain(`${api}/v1/price`);
    expect(out.llms).not.toContain(MAINNET);
  });

  it('leaves a mainnet build as shipped', () => {
    const out = run();
    expect(out.status).toBe(0);
    expect(out.redirects).toContain(`${MAINNET}/v1/:splat`);
    expect(out.llms).toContain(MAINNET);
  });

  it('refuses an API base that is not a bare origin', () => {
    expect(run('https://api.testnet.stellarindex.io/x|y').status).toBe(1);
  });
});
