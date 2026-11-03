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
  const topBids = bids.slice(0, 8);
  const topAsks = asks.slice(0, 8);

  const totalBidQty = bids.reduce((acc, l) => acc + l.qty, 0);
  const totalAskQty = asks.reduce((acc, l) => acc + l.qty, 0);
  const combinedQty = totalBidQty + totalAskQty;
  const buyPct = combinedQty > 0 ? Math.round((totalBidQty / combinedQty) * 100) : 50;
  const sellPct = combinedQty > 0 ? 100 - buyPct : 50;

  const maxQty = Math.max(
    1,
    ...topBids.map((l) => l.qty),
    ...topAsks.map((l) => l.qty)
  );

  const bestBid = bids[0]?.price;
  const bestAsk = asks[0]?.price;
  const spread = bestBid && bestAsk ? bestAsk - bestBid : null;

  return (
    <div className="groww-card orderbook-panel" data-testid="orderbook">
      <div className="groww-section-header">
        <div>
          <h3 className="groww-section-title">Market Depth</h3>
          <span className="groww-section-sub">
            {symbol} L2 Order Book · Click any price level to pre-fill ticket
          </span>
        </div>
        <div className="groww-depth-badges">
          {lastPrice ? (
            <span className="groww-pill-tag mono">LTP ${formatPrice(lastPrice)}</span>
          ) : null}
          <span className="groww-pill-tag mono muted">
            {spread !== null ? `Spread $${formatPrice(spread)}` : 'Spread —'}
          </span>
        </div>
      </div>

      {/* Groww Buy vs Sell Sentiment Bar */}
      <div className="groww-sentiment-box">
        <div className="groww-sentiment-labels">
          <span className="buy-text">
            Buy orders <strong className="mono">{buyPct}%</strong>
          </span>
          <span className="sell-text">
            <strong className="mono">{sellPct}%</strong> Sell orders
          </span>
        </div>
        <div className="groww-sentiment-bar">
          <div
            className="groww-sentiment-buy"
            style={{ width: `${buyPct}%` }}
          />
          <div
            className="groww-sentiment-sell"
            style={{ width: `${sellPct}%` }}
          />
        </div>
      </div>

      {/* Groww Side-by-Side Bid / Ask Table */}
      <div className="groww-depth-grid">
        {/* Bids Column */}
        <div className="groww-depth-col">
          <div className="groww-depth-head">
            <span>Bid Price</span>
            <span className="text-center">Orders</span>
            <span className="text-right">Qty</span>
          </div>
          <div className="groww-depth-rows">
            {topBids.length === 0 ? (
              <div className="empty-row">No resting bids</div>
            ) : (
              topBids.map((lvl) => {
                const pct = Math.min(100, (lvl.qty / maxQty) * 100);
                return (
                  <div
                    key={`bid-${lvl.price}`}
                    className="groww-depth-row"
                    onClick={() => onSelectPrice?.(lvl.price)}
                  >
                    <div
                      className="depth-bar bid-bar"
                      style={{ width: `${pct}%` }}
                    />
                    <span className="mono bid-px">${formatPrice(lvl.price)}</span>
                    <span className="mono muted text-center">{lvl.orders}</span>
                    <span className="mono text-right">{lvl.qty.toLocaleString()}</span>
                  </div>
                );
              })
            )}
          </div>
          <div className="groww-depth-footer">
            <span>Bid Total</span>
            <span className="mono bold">{totalBidQty.toLocaleString()}</span>
          </div>
        </div>

        {/* Asks Column */}
        <div className="groww-depth-col">
          <div className="groww-depth-head">
            <span>Ask Price</span>
            <span className="text-center">Orders</span>
            <span className="text-right">Qty</span>
          </div>
          <div className="groww-depth-rows">
            {topAsks.length === 0 ? (
              <div className="empty-row">No resting asks</div>
            ) : (
              topAsks.map((lvl) => {
                const pct = Math.min(100, (lvl.qty / maxQty) * 100);
                return (
                  <div
                    key={`ask-${lvl.price}`}
                    className="groww-depth-row"
                    onClick={() => onSelectPrice?.(lvl.price)}
                  >
                    <div
                      className="depth-bar ask-bar"
                      style={{ width: `${pct}%` }}
                    />
                    <span className="mono ask-px">${formatPrice(lvl.price)}</span>
                    <span className="mono muted text-center">{lvl.orders}</span>
                    <span className="mono text-right">{lvl.qty.toLocaleString()}</span>
                  </div>
                );
              })
            )}
          </div>
          <div className="groww-depth-footer">
            <span>Ask Total</span>
            <span className="mono bold">{totalAskQty.toLocaleString()}</span>
          </div>
        </div>
      </div>
    </div>
  );
};
