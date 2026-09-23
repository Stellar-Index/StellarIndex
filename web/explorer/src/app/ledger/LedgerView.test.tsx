import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { LedgerView } from './LedgerView';

const apiGet = vi.hoisted(() => vi.fn());
vi.mock('@/api/client', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/api/client')>()),
  apiGet,
}));
vi.mock('next/navigation', () => ({
  useSearchParams: () => new URLSearchParams(),
}));

// CURRENT_NETWORK_ID is resolved at module load; the caption helper reads it
// at CALL time, so a getter-backed mock lets each case pick the network
// without resetting the module graph.
const net = vi.hoisted(() => ({ id: 'mainnet' as 'mainnet' | 'testnet' }));
vi.mock('@/lib/networks', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/lib/networks')>();
  return {
    ...actual,
    get CURRENT_NETWORK_ID() {
      return net.id;
    },
    get CURRENT_NETWORK() {
      return actual.NETWORKS.find((n) => n.id === net.id)!;
    },
  };
});

afterEach(() => {
  apiGet.mockReset();
  net.id = 'mainnet';
});

// Measured live 2026-08-28: mainnet total_coins still counts the 2019 burn.
const LEDGER = {
  sequence: 60_000_000,
  hash: 'a'.repeat(64),
  prev_hash: 'b'.repeat(64),
  close_time: '2026-08-28T00:00:00Z',
  protocol_version: 23,
  base_fee: '100',
  base_reserve: '5000000',
  total_coins: '1054439020873472865', // ~105.4B XLM
  fee_pool: '104692050458598',
  tx_count: 1,
  op_count: 1,
  soroban_event_count: 0,
};

function routeApi() {
  apiGet.mockImplementation(async (path: string) => {
    if (path === `/v1/ledgers/${LEDGER.sequence}`) return { data: LEDGER };
    if (path === `/v1/ledgers/${LEDGER.sequence}/transactions`)
      return { data: { transactions: [] } };
    throw new Error(`unexpected ${path}`);
  });
}

function renderView() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <LedgerView seq={String(LEDGER.sequence)} />
    </QueryClientProvider>,
  );
}

describe('LedgerView total_coins caption', () => {
  it('captions the mainnet header total_coins as including the 2019 burn', async () => {
    routeApi();
    renderView();
    const value = await screen.findByText('105,443,902,087.3472865 XLM');
    const cell = value.closest('dd')!;
    expect(cell).toHaveTextContent('ledger header · includes the 2019 burn');
  });

  it('does not claim a burn on testnet', async () => {
    net.id = 'testnet';
    routeApi();
    renderView();
    const value = await screen.findByText('105,443,902,087.3472865 XLM');
    const cell = value.closest('dd')!;
    expect(cell).toHaveTextContent('ledger header');
    expect(cell).not.toHaveTextContent('burn');
  });
});

describe('LedgerView count basis captions', () => {
  // tx_count/op_count cover the whole tx set, failed txs included, while
  // soroban_event_count covers applied txs only: each figure states its basis.
  it('labels which counts include failed transactions', async () => {
    routeApi();
    renderView();
    const cellFor = async (label: string) => {
      const dts = await screen.findAllByText(label);
      const dt = dts.find((el) => el.tagName === 'DT');
      return dt!.parentElement!.querySelector('dd')!;
    };
    expect(await cellFor('Transactions')).toHaveTextContent(
      'tx set · incl. failed',
    );
    expect(await cellFor('Operations')).toHaveTextContent(
      'tx set · incl. ops of failed txs',
    );
    expect(await cellFor('Soroban events')).toHaveTextContent(
      'successful txs only',
    );
  });
});

describe('LedgerView stroop fields', () => {
  it('renders the string base_fee and base_reserve the API serves', async () => {
    routeApi();
    renderView();
    expect(await screen.findByText('100 stroops')).toBeInTheDocument();
    expect(screen.getByText('0.5 XLM')).toBeInTheDocument();
  });
});
