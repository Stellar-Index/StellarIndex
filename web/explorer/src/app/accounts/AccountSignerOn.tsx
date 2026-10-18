'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { Table, TableWrap, TBody, Td, Th, THead, TR } from '@/components/ui';
import { apiGet, asExample } from '@/api/client';
import { truncateMiddle } from '@/lib/format';
import { type Envelope } from '../explorer-shared';

interface SignerOn {
  account: string;
  weight: number;
  last_modified_ledger: number;
}

interface SignerOnResp {
  account: string;
  signer_on: SignerOn[];
  through_ledger: number;
  lower_bound: boolean;
  coverage_note: string;
}

/**
 * AccountSignerOnPanel lists the other accounts whose current signers
 * include this account's key. It renders nothing when there are none or the
 * signer index is not built yet (503), like the other entry panels.
 */
export function AccountSignerOnPanel({ id }: { id: string }) {
  const { data } = useQuery<SignerOnResp>({
    queryKey: ['/v1/accounts/{id}/signer-on', id],
    enabled: id.length > 0,
    retry: false,
    queryFn: async () => {
      const env = await apiGet<Envelope<SignerOnResp>>(
        `/v1/accounts/${encodeURIComponent(id)}/signer-on`,
      );
      return env.data;
    },
    staleTime: 30_000,
  });
  const rows = data?.signer_on ?? [];
  if (rows.length === 0) return null;
  return (
    <Panel
      title={`Signer on (${rows.length}${data?.lower_bound ? '+' : ''})`}
      hint={
        data?.lower_bound
          ? data.coverage_note
          : 'Other accounts this account’s key can sign for'
      }
      source={asExample(`/v1/accounts/${id}/signer-on`)}
      bodyClassName="-mx-4 -mb-4"
    >
      <TableWrap>
        <Table>
          <THead>
            <tr>
              <Th>Account</Th>
              <Th align="right">Weight</Th>
              <Th align="right">Updated</Th>
            </tr>
          </THead>
          <TBody>
            {rows.map((s) => (
              <TR key={s.account}>
                <Td>
                  <Link
                    href={`/accounts/${encodeURIComponent(s.account)}/`}
                    className="text-ink hover:text-brand-600 font-mono"
                    title={s.account}
                  >
                    {truncateMiddle(s.account, 10, 8)}
                  </Link>
                </Td>
                <Td align="right" className="font-mono tabular-nums">
                  {s.weight}
                </Td>
                <Td align="right" className="text-ink-muted font-mono text-xs">
                  {`ledger ${s.last_modified_ledger}`}
                </Td>
              </TR>
            ))}
          </TBody>
        </Table>
      </TableWrap>
    </Panel>
  );
}
