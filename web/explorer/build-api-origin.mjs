// The API origin a build talks to: NEXT_PUBLIC_API_BASE_URL when set, else the origin of NEXT_PUBLIC_NETWORK.
// next.config.mjs and scripts/ci/explorer-network-hosts.sh both resolve it here, so a test-net build configured
// with NEXT_PUBLIC_NETWORK alone is not treated as mainnet. A test holds the table to src/lib/networks.ts.
import { fileURLToPath } from 'node:url';

export const NETWORK_API_ORIGINS = {
  mainnet: 'https://api.stellarindex.io',
  testnet: 'https://api.testnet.stellarindex.io',
  futurenet: 'https://api.futurenet.stellarindex.io',
};

export function buildApiOrigin(env = process.env) {
  const explicit = (env.NEXT_PUBLIC_API_BASE_URL ?? '').replace(/\/+$/, '');
  if (explicit) return explicit;
  return (
    NETWORK_API_ORIGINS[env.NEXT_PUBLIC_NETWORK] ?? NETWORK_API_ORIGINS.mainnet
  );
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  console.log(buildApiOrigin());
}
