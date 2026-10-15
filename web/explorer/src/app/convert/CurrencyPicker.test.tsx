import { describe, it, expect, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';

import { CurrencyPicker } from './CurrencyPicker';

const GROUPS = [
  { label: 'Stellar', items: [{ ticker: 'XLM', name: 'Stellar Lumens' }] },
  {
    label: 'Verified Stellar assets',
    items: [{ ticker: 'USDC', name: 'USD Coin' }],
  },
  { label: 'Fiat currencies', items: [{ ticker: 'USD' }, { ticker: 'EUR' }] },
];

describe('CurrencyPicker', () => {
  it('opens a searchable list and picks on click', () => {
    const onPick = vi.fn();
    render(
      <CurrencyPicker
        label="From"
        value="XLM"
        groups={GROUPS}
        onPick={onPick}
      />,
    );
    expect(screen.queryByRole('listbox')).toBeNull();
    fireEvent.click(screen.getByLabelText('From'));
    expect(screen.getAllByRole('option')).toHaveLength(4);
    fireEvent.change(screen.getByRole('combobox'), {
      target: { value: 'coin' },
    });
    const options = screen.getAllByRole('option');
    expect(options).toHaveLength(1);
    expect(options[0]).toHaveTextContent('USDC');
    fireEvent.click(options[0]);
    expect(onPick).toHaveBeenCalledWith('USDC');
    expect(screen.queryByRole('listbox')).toBeNull();
  });

  it('walks the list with the arrow keys and picks on Enter', () => {
    const onPick = vi.fn();
    render(
      <CurrencyPicker label="To" value="USD" groups={GROUPS} onPick={onPick} />,
    );
    fireEvent.click(screen.getByLabelText('To'));
    const input = screen.getByRole('combobox');
    fireEvent.change(input, { target: { value: 'u' } });
    expect(screen.getAllByRole('option').map((o) => o.textContent)).toEqual([
      'XLM Stellar Lumens',
      'USDC USD Coin',
      'USD',
      'EUR',
    ]);
    fireEvent.keyDown(input, { key: 'ArrowDown' });
    fireEvent.keyDown(input, { key: 'ArrowDown' });
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(onPick).toHaveBeenCalledWith('USD');
  });

  it('closes on Escape without picking', () => {
    const onPick = vi.fn();
    render(
      <CurrencyPicker label="To" value="USD" groups={GROUPS} onPick={onPick} />,
    );
    fireEvent.click(screen.getByLabelText('To'));
    fireEvent.keyDown(screen.getByRole('combobox'), { key: 'Escape' });
    expect(screen.queryByRole('listbox')).toBeNull();
    expect(onPick).not.toHaveBeenCalled();
    expect(screen.getByLabelText('To')).toHaveTextContent('USD');
  });
});
