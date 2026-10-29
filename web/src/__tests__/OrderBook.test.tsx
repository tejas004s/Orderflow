import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import { OrderBook } from '../components/OrderBook';

describe('OrderBook', () => {
  it('renders bids, asks, spread, and invokes onSelectPrice on click', () => {
    const onSelect = vi.fn();
    render(
      <OrderBook
        symbol="AAPL"
        bids={[{ price: 18890, qty: 15, orders: 2 }]}
        asks={[{ price: 18910, qty: 20, orders: 3 }]}
        lastPrice={18900}
        onSelectPrice={onSelect}
      />
    );

    expect(screen.getByText('Order Book · AAPL')).toBeDefined();
    expect(screen.getByText('188.90')).toBeDefined();
    expect(screen.getByText('189.10')).toBeDefined();
    expect(screen.getByText('Spread $0.20')).toBeDefined();

    fireEvent.click(screen.getByText('189.10'));
    expect(onSelect).toHaveBeenCalledWith(18910);
  });
});
