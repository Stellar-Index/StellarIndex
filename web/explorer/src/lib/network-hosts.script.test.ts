import { spawnSync } from 'node:child_process';
import { cpSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import { NETWORKS } from './networks';

// The script the build runs over out/: public/_redirects and public/llms.txt
// name the mainnet API and must not ship that way into another network.
const SCRIPT = join(
  process.cwd(),
  '../../scripts/ci/explorer-network-hosts.sh',
);
const MAINNET = 'https://api.stellarindex.io';

let dir = '';
afterEach(() => {
  if (dir) rmSync(dir, { recursive: true, force: true });
});

function run(apiBase?: string, network?: string) {
  dir = mkdtempSync(join(tmpdir(), 'hosts-'));
  for (const f of ['_redirects', 'llms.txt']) {
    cpSync(join(process.cwd(), 'public', f), join(dir, f));
  }
  const env = { ...process.env };
  delete env.NEXT_PUBLIC_API_BASE_URL;
  delete env.NEXT_PUBLIC_NETWORK;
  if (apiBase) env.NEXT_PUBLIC_API_BASE_URL = apiBase;
  if (network) env.NEXT_PUBLIC_NETWORK = network;
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

  // The runbook makes NEXT_PUBLIC_API_BASE_URL optional on a test net.
  it('points a build configured by NEXT_PUBLIC_NETWORK alone at that network', () => {
    const api = 'https://api.futurenet.stellarindex.io';
    const out = run(undefined, 'futurenet');
    expect(out.status).toBe(0);
    expect(out.redirects).toContain(`/v1/*     ${api}/v1/:splat 301`);
    expect(out.redirects).not.toContain(MAINNET);
    expect(out.llms).not.toContain(MAINNET);
  });

  it('resolves the same origins the app does', () => {
    const env = { ...process.env };
    delete env.NEXT_PUBLIC_API_BASE_URL;
    for (const n of NETWORKS) {
      const res = spawnSync(
        'node',
        [join(process.cwd(), 'build-api-origin.mjs')],
        {
          env: { ...env, NEXT_PUBLIC_NETWORK: n.id },
          encoding: 'utf8',
        },
      );
      expect(res.stdout.trim()).toBe(n.apiBaseUrl);
    }
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
