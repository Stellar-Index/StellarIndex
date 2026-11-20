'use client';

import Link from 'next/link';

import { Panel } from '@/components/reveal';
import { Fact, FactList } from '@/components/ui/Facts';
import { CopyButton } from '@/components/ui';
import { asExample } from '@/api/client';
import { formatCompactUnits, truncateMiddle } from '@/lib/format';
import { fixedPct, poolStatusText } from '../poolConfig';
import { usePoolReserves } from './usePoolReserves';

function addressHref(a: string): string {
  const root = a.startsWith('C') ? 'contracts' : 'accounts';
  return `/${root}/${encodeURIComponent(a)}/`;
}

function AddressValue({ value }: { value: string | undefined }) {
  if (!value) return <>—</>;
  return (
    <span className="inline-flex items-center gap-1 font-mono">
      <Link
        href={addressHref(value)}
        className="text-brand-600 hover:underline"
        title={value}
      >
        {truncateMiddle(value, 6, 6)}
      </Link>
      <CopyButton value={value} />
    </span>
  );
}

/** Raw i128 in the oracle's base unit, which the API does not name: compact, exact on hover. */
function minCollateralText(raw: string): string {
  return `${formatCompactUnits(raw)} base units`;
}

export function PoolDetails({ pool }: { pool: string }) {
  const q = usePoolReserves(pool);
  const cfg = q.data?.pool_config;
  if (!cfg) return null;
  const status = poolStatusText(cfg);
  return (
    <Panel
      headingLevel={2}
      title="Pool details"
      source={asExample(`/v1/lending/pools/${pool}/reserves`, {})}
    >
      <FactList>
        <Fact
          label="Status"
          title={cfg.status != null ? `code ${cfg.status}` : undefined}
        >
          {status ?? '—'}
        </Fact>
        <Fact label="Oracle">
          <AddressValue value={cfg.oracle} />
        </Fact>
        <Fact label="Admin">
          <AddressValue value={cfg.admin} />
        </Fact>
        <Fact label="Backstop take rate" title={cfg.backstop_take_rate}>
          {fixedPct(cfg.backstop_take_rate, q.data?.config_decimals)}
        </Fact>
        <Fact label="Max positions">
          {cfg.max_positions != null
            ? cfg.max_positions.toLocaleString('en-US')
            : '—'}
        </Fact>
        <Fact
          label="Min collateral"
          title={cfg.min_collateral && `${cfg.min_collateral} base units`}
        >
          {cfg.min_collateral != null
            ? minCollateralText(cfg.min_collateral)
            : '—'}
        </Fact>
      </FactList>
    </Panel>
  );
}
