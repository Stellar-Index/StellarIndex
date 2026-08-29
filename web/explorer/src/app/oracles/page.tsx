import type { Metadata } from 'next';

import { OraclesView } from './OraclesView';

export const metadata: Metadata = {
  title: 'Oracles — on-chain price feeds for Stellar',
  description:
    'Reflector trio (DEX/CEX/FX), Redstone, Band — every Stellar oracle we ingest. Per-oracle 24h activity table + every active price stream, with unmapped raw feeds listed separately. Source: /v1/sources?class=oracle and /v1/oracle/streams?include_unmapped=true.',
  alternates: { canonical: '/oracles' },
};

export default function OraclesPage() {
  return <OraclesView />;
}
