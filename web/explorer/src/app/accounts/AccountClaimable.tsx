'use client';

import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { AssetLink } from '@/components/AssetLink';
import { Table, TableWrap, TBody, Td, Th, THead, TR } from '@/components/ui';
import { apiGet, asExample } from '@/api/client';
import { truncateMiddle } from '@/lib/format';
import {
  type Envelope,
  formatTimestamp,
  xlmReadable,
} from '../explorer-shared';

interface Claimant {
  destination: string;
  predicate?: { type?: string };
}

export interface ClaimableBalance {
  balance_id: string;
  asset: string;
  amount: string;
  sponsor?: string;
  last_modified_ledger: number;
  last_modified_time: string;
  entry: { claimants?: Claimant[] };
}

interface ClaimableResp {
  account: string;
  claimable_balances: ClaimableBalance[];
  through_ledger: number;
  lower_bound: boolean;
  coverage_note: string;
}

/** claimCondition is "now" for an unconditional claim, else "conditional" (a time or logic predicate the API serves in full). */
export function claimCondition(cb: ClaimableBalance, account: string): string {
  const mine = cb.entry.claimants?.find((c) => c.destination === account);
  return mine?.predicate?.type === 'unconditional' ? 'now' : 'conditional';
}

/**
 * AccountClaimablePanel lists the unclaimed claimable balances this account
 * can claim. It renders nothing when there are none or the route is not
 * available yet (entry index unbuilt), like the other entry panels.
 */
export function AccountClaimablePanel({ id }: { id: string }) {
  const { data } = useQuery<ClaimableResp>({
    queryKey: ['/v1/accounts/{id}/claimable-balances', id],
    enabled: id.length > 0,
    retry: false,
    queryFn: async () => {
      const env = await apiGet<Envelope<ClaimableResp>>(
        `/v1/accounts/${encodeURIComponent(id)}/claimable-balances`,
      );
      return env.data;
    },
    staleTime: 30_000,
  });
  const rows = data?.claimable_balances ?? [];
  if (rows.length === 0) return null;
  return (
    <Panel
      title={`Claimable balances (${rows.length}${data?.lower_bound ? '+' : ''})`}
      hint={
        data?.lower_bound
          ? data.coverage_note
          : 'Unclaimed balances this account can claim'
      }
      source={asExample(`/v1/accounts/${id}/claimable-balances`)}
      bodyClassName="-mx-4 -mb-4"
    >
      <TableWrap>
        <Table>
          <THead>
            <tr>
              <Th>Asset</Th>
              <Th align="right">Amount</Th>
              <Th>Claimable</Th>
              <Th>Balance ID</Th>
              <Th align="right">Updated</Th>
            </tr>
          </THead>
          <TBody>
            {rows.map((cb) => (
              <TR key={cb.balance_id}>
                <Td>
                  <AssetLink canonical={cb.asset} />
                </Td>
                <Td align="right" className="font-mono tabular-nums">
                  {xlmReadable(cb.amount)}
                </Td>
                <Td className="text-ink-muted text-xs">
                  {claimCondition(cb, id)}
                </Td>
                <Td className="font-mono text-xs" title={cb.balance_id}>
                  {truncateMiddle(cb.balance_id, 10, 6)}
                </Td>
                <Td
                  align="right"
                  className="text-ink-muted text-xs"
                  title={`ledger ${cb.last_modified_ledger}`}
                >
                  {formatTimestamp(cb.last_modified_time)}
                </Td>
              </TR>
            ))}
          </TBody>
        </Table>
      </TableWrap>
    </Panel>
  );
}
