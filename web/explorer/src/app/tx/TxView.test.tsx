import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { TxView } from './TxView';

const H = 'a'.repeat(64);
const PAYER = 'GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ';

function renderTx(data: object) {
  vi.mocked(apiGet).mockResolvedValue({ data });
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  render(
    <QueryClientProvider client={client}>
      <TxView hash={H} />
    </QueryClientProvider>,
  );
}

const base = {
  hash: H,
  ledger: 1,
  close_time: '2026-10-10T00:00:00Z',
  index: 0,
  source_account: PAYER,
  fee_charged: '200',
  max_fee: '1000',
  operation_count: 2,
  result_code: 0,
};

describe('TxView variants', () => {
  it('names the failing op and its inner reason on a failed tx', async () => {
    renderTx({
      ...base,
      successful: false,
      result: 'tx_failed',
      result_code: -1,
      operations: [
        { op_index: 0, type: 'payment', result_code: 0, result: 'op_success' },
        {
          op_index: 1,
          type: 'payment',
          result_code: -1,
          result: 'op_inner',
          inner_result: 'payment_underfunded',
        },
      ],
      events: [],
    });
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Failed at op #1: payment_underfunded',
    );
    expect(
      screen.getByText('Failed transaction: any events were rolled back.'),
    ).toBeInTheDocument();
  });

  it('shows the fee payer and inner max fee on a fee bump', async () => {
    renderTx({
      ...base,
      successful: true,
      result: 'tx_fee_bump_inner_success',
      fee_bump: {
        fee_account: PAYER,
        inner_hash: 'b'.repeat(64),
        inner_max_fee: '500',
      },
      operations: [],
    });
    expect(await screen.findByText('Max fee (fee payer)')).toBeInTheDocument();
    expect(screen.getByText('Max fee (inner)')).toBeInTheDocument();
    expect(screen.getByText('Fee bump')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('labels a non-UTF-8 text memo as base64', async () => {
    renderTx({
      ...base,
      successful: true,
      result: 'tx_success',
      memo_type: 'text',
      memo_base64: '/w==',
      operations: [],
    });
    expect(await screen.findByText('/w==')).toBeInTheDocument();
    expect(screen.getByText('base64')).toBeInTheDocument();
  });
});
