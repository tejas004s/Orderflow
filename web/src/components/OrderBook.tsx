import React from 'react';
import { formatPrice, type Level } from '../types';

interface OrderBookProps {
  symbol: string;
  bids: Level[];
  asks: Level[];
  lastPrice?: number;
  onSelectPrice?: (priceCents: number) => void;
}

export const OrderBook: React.FC<OrderBookProps> = ({
  symbol,
  bids,
  asks,
  lastPrice,
  onSelectPrice,
}) => {
  const topAsks = asks.slice(0, 10).slice().reverse(); // highest ask at top, best ask right above spread
  const topBids = bids.slice(0, 10);

  const maxQty = Math.max(
    1,
    ...topAsks.map((l) => l.qty),
    ...topBids.map((l) => l.qty)
  );

  const bestBid = bids[0]?.price;
  const bestAsk = asks[0]?.price;
  const spread = bestBid && bestAsk ? bestAsk - bestBid : null;

  return (
    <div className="panel orderbook-panel" data-testid="orderbook">
      <div className="panel-header">
        <span>Order Book · {symbol}</span>
        <span className="muted">L2 Depth</span>
      </div>
      <div className="ob-cols-header">
        <span>Price (USD)</span>
        <span>Size</span>
        <span>Orders</span>
      </div>

      <div className="ob-side ob-asks">
        {topAsks.length === 0 ? (
          <div className="empty-row">No asks</div>
        ) : (
          topAsks.map((lvl) => {
            const pct = Math.min(100, (lvl.qty / maxQty) * 100);
            return (
              <div
                key={`ask-${lvl.price}`}
                className="ob-row ask-row"
                onClick={() => onSelectPrice?.(lvl.price)}
              >
                <div className="depth-bar ask-bar" style={{ width: `${pct}%` }} />
                <span className="mono ask-px">{formatPrice(lvl.price)}</span>
                <span className="mono">{lvl.qty.toLocaleString()}</span>
                <span className="mono muted">{lvl.orders}</span>
              </div>
            );
          })
        )}
      </div>

      <div className="ob-spread">
        <span className="last-px mono">
          {lastPrice ? `$${formatPrice(lastPrice)}` : '—'}
        </span>
        <span className="spread-badge mono">
          {spread !== null ? `Spread $${formatPrice(spread)}` : 'Spread —'}
        </span>
      </div>

      <div className="ob-side ob-bids">
        {topBids.length === 0 ? (
          <div className="empty-row">No bids</div>
        ) : (
          topBids.map((lvl) => {
            const pct = Math.min(100, (lvl.qty / maxQty) * 100);
            return (
              <div
                key={`bid-${lvl.price}`}
                className="ob-row bid-row"
                onClick={() => onSelectPrice?.(lvl.price)}
              >
                <div className="depth-bar bid-bar" style={{ width: `${pct}%` }} />
                <span className="mono bid-px">{formatPrice(lvl.price)}</span>
                <span className="mono">{lvl.qty.toLocaleString()}</span>
                <span className="mono muted">{lvl.orders}</span>
              </div>
            );
          })
        )}
      </div>
    </div>
  );
};
