'use client';

import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { Stat, StatCell, StatGrid } from '@/components/ui';
import { apiGet, asExample } from '@/api/client';
import type { operations } from '@/api/types';
import type { Envelope } from '../explorer-shared';

type SponsorshipsResp = NonNullable<
  operations['getAccountSponsorships']['responses'][200]['content']['application/json']['data']
>;

const numFmt = new Intl.NumberFormat('en-US');

/**
 * AccountSponsorshipsPanel — the sponsorships in force now, beside the graph's started-only history: totals for both sides
 * and the account entry's own counters checked against them.
 */
export function AccountSponsorshipsPanel({ id }: { id: string }) {
  // limit=1: the panel reads only totals and proof, which cover every arrangement whatever the page size.
  const params = { limit: 1 };
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['account', id, 'sponsorships-totals'],
    enabled: id.length > 0,
    retry: false,
    queryFn: async () => {
      const env = await apiGet<Envelope<SponsorshipsResp>>(
        `/v1/accounts/${encodeURIComponent(id)}/sponsorships`,
        params,
      );
      return env.data;
    },
    staleTime: 60_000,
  });

  const source = asExample(`/v1/accounts/${id}/sponsorships`, params);
  const title = 'Sponsorships in force';

  if (isError) {
    const notBuilt =
      error instanceof Error &&
      error.message.includes('sponsorships-not-built');
    return (
      <Panel
        title={title}
        source={source}
        bodyClassName="text-sm text-ink-body"
      >
        {notBuilt
          ? 'The sponsorship lifecycle has not been derived on this deployment yet.'
          : `Sponsorships failed to load — reload to retry${error instanceof Error ? `: ${error.message}` : ''}.`}
      </Panel>
    );
  }
  if (isLoading || !data) {
    return (
      <Panel
        title={title}
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        Loading…
      </Panel>
    );
  }

  const { sponsoring, sponsored } = data.totals;
  const { proof } = data;
  const proofText =
    proof.match === true
      ? 'The account entry’s own counters agree with these totals.'
      : proof.match === false
        ? `The account entry counts ${numFmt.format(proof.num_sponsoring)} sponsoring and ${numFmt.format(proof.num_sponsored)} sponsored reserves; the derived totals disagree.`
        : 'The account entry changed after the derivation, so its counters are not compared.';

  return (
    <Panel title={title} source={source} bodyClassName="space-y-4">
      <StatGrid>
        <StatCell>
          <Stat
            label="Sponsoring now"
            value={numFmt.format(sponsoring.in_force)}
            sub={`${numFmt.format(sponsoring.in_force_reserve_count)} reserves`}
          />
        </StatCell>
        <StatCell>
          <Stat
            label="Sponsored now"
            value={numFmt.format(sponsored.in_force)}
            sub={`${numFmt.format(sponsored.in_force_reserve_count)} reserves`}
          />
        </StatCell>
        <StatCell>
          <Stat
            label="Ever sponsoring"
            value={numFmt.format(sponsoring.started)}
            sub={`${numFmt.format(sponsoring.ended)} ended · ${numFmt.format(sponsoring.revoked)} revoked`}
          />
        </StatCell>
        <StatCell>
          <Stat
            label="Ever sponsored"
            value={numFmt.format(sponsored.started)}
            sub={`${numFmt.format(sponsored.ended)} ended · ${numFmt.format(sponsored.revoked)} revoked`}
          />
        </StatCell>
      </StatGrid>
      <p className="text-ink-muted text-xs">
        {proofText} Derived through ledger #{data.through_ledger}.
      </p>
    </Panel>
  );
}
