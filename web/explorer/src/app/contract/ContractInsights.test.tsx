import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import {
  CodeTimeline,
  codeTimelineSegments,
  tokenFlowSummary,
  TokenFlowSummary,
} from './ContractInsights';

const NOW = Date.parse('2026-01-11T00:00:00Z');
const v = (ledger: number, close_time: string, wasm_hash = `h${ledger}`) => ({
  ledger,
  close_time,
  wasm_hash,
});

describe('codeTimelineSegments', () => {
  it('splits the deploy-to-now span by each version’s time in service', () => {
    const segs = codeTimelineSegments(
      [v(1, '2026-01-01T00:00:00Z'), v(2, '2026-01-09T00:00:00Z')],
      NOW,
    );
    expect(segs.map((s) => s.share)).toEqual([0.8, 0.2]);
    expect(segs[0]!.end).toBe('2026-01-09T00:00:00Z');
    expect(segs[1]!.end).toBeNull();
  });

  it.each([
    ['a missing time', [v(1, '2026-01-01T00:00:00Z'), v(2, '')]],
    [
      'out-of-order times',
      [v(1, '2026-01-09T00:00:00Z'), v(2, '2026-01-01T00:00:00Z')],
    ],
    ['no versions', []],
  ])('guesses no span for %s', (_, versions) => {
    expect(codeTimelineSegments(versions, NOW)).toEqual([]);
  });

  it('draws nothing for a single version', () => {
    const { container } = render(
      <CodeTimeline versions={[v(1, '2026-01-01T00:00:00Z')]} now={NOW} />,
    );
    expect(container.innerHTML).toBe('');
  });
});

describe('tokenFlowSummary', () => {
  const A = 'GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA';
  const B = 'GBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB';

  it('sums transfers exactly and never sums an approve allowance', () => {
    const s = tokenFlowSummary([
      { event_kind: 'transfer', from: A, to: B, amount: '9007199254740993' },
      { event_kind: 'transfer', from: A, to: B, amount: '1' },
      { event_kind: 'approve', from: B, to: A, amount: '500' },
    ]);
    expect(s.senders).toEqual([
      { address: A, raw: 9007199254740994n, count: 2 },
    ]);
    expect(s.receivers.map((p) => p.address)).toEqual([B]);
    expect(s.kinds).toEqual([
      { kind: 'transfer', count: 2 },
      { kind: 'approve', count: 1 },
    ]);
  });

  it('skips a non-integer amount rather than parsing it', () => {
    const s = tokenFlowSummary([
      { event_kind: 'transfer', from: A, to: B, amount: '1.5' },
    ]);
    expect(s.senders).toEqual([]);
    expect(s.kinds).toEqual([{ kind: 'transfer', count: 1 }]);
  });

  it('labels the panel as the newest rows only', () => {
    render(
      <TokenFlowSummary
        rows={[{ event_kind: 'transfer', from: A, to: B, amount: '10' }]}
        decimals={0}
        formatAmount={(raw) => raw}
      />,
    );
    expect(screen.getByTestId('token-flow-summary').textContent).toContain(
      'Over the 1 newest audit-trail rows only, not all-time.',
    );
  });
});
