import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { TxStatusBadge } from './TxStatusBadge';

describe('TxStatusBadge', () => {
  it('renders a green "success" pill when the outcome applied', () => {
    render(<TxStatusBadge successful={true} result="tx_success" code={0} />);
    const badge = screen.getByText('success');
    expect(badge).toHaveClass('bg-up-subtle');
    expect(badge).toHaveClass('text-up-strong');
    expect(badge).toHaveAttribute('title', 'success');
  });

  it('renders the human reason slug in red on failure, code kept in the title', () => {
    render(<TxStatusBadge successful={false} result="tx_insufficient_fee" code={-6} />);
    // The reason slug is the visible label — a failed tx reads as its reason,
    // never as "success" and never as a bare number.
    const badge = screen.getByText('tx_insufficient_fee');
    expect(badge).toHaveClass('bg-down-subtle');
    expect(badge).toHaveClass('text-down-strong');
    // Full slug + numeric code preserved in the title.
    expect(badge).toHaveAttribute('title', 'tx_insufficient_fee (code -6)');
  });

  it('falls back to "code N" on failure when no slug is present', () => {
    render(<TxStatusBadge successful={false} code={-1} />);
    const badge = screen.getByText('code -1');
    expect(badge).toHaveClass('bg-down-subtle');
    expect(badge).toHaveAttribute('title', 'code -1');
  });

  it('renders a MUTED "unknown" for an undefined outcome — never success', () => {
    render(<TxStatusBadge successful={undefined} />);
    const badge = screen.getByText('unknown');
    // The honest degraded state: muted, with the disclosure title …
    expect(badge).toHaveClass('text-ink-muted');
    expect(badge).toHaveAttribute('title', 'transaction outcome unavailable');
    // … and explicitly NOT the success (green) treatment.
    expect(badge).not.toHaveClass('bg-up-subtle');
    expect(badge).not.toHaveClass('text-up-strong');
    expect(screen.queryByText('success')).not.toBeInTheDocument();
  });
});
