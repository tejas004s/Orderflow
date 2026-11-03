import React from 'react';
import { formatPrice, type Trade } from '../types';

interface TradeTapeProps {
  symbol: string;
  trades: Trade[];
}

function formatTradeTime(ts: string | number): string {
  if (typeof ts === 'number') {
    const ms = ts > 1e14 ? Math.floor(ts / 1e6) : ts;
    return new Date(ms).toLocaleTimeString();
  }
  const d = new Date(ts);
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleTimeString();
}

export const TradeTape: React.FC<TradeTapeProps> = ({ symbol, trades }) => {
  return (
    <div className="groww-card tape-panel">
      <div className="groww-section-header">
        <div>
          <h3 className="groww-section-title">Market Trades</h3>
          <span className="groww-section-sub">Real-time execution stream · {symbol}</span>
        </div>
        <span className="groww-live-pill">
          <span className="groww-live-dot" /> LIVE
        </span>
      </div>

      <div className="groww-depth-head" style={{ padding: '8px 16px' }}>
        <span>Price</span>
        <span className="text-center">Qty</span>
        <span className="text-right">Time</span>
      </div>

      <div className="tape-list">
        {trades.length === 0 ? (
          <div className="empty-row">Waiting for executions…</div>
        ) : (
          trades.slice(0, 28).map((tr, idx) => (
            <div key={tr.match_id || idx} className="groww-tape-row">
              <span
                className={`mono bold ${
                  tr.aggressor_side === 'BUY' ? 'bid-px' : 'ask-px'
                }`}
              >
                ${formatPrice(tr.price)}
              </span>
              <span className="mono text-center">{tr.qty.toLocaleString()}</span>
              <span className="mono muted text-right">
                {formatTradeTime(tr.executed_at)}
              </span>
            </div>
          ))
        )}
      </div>
    </div>
  );
};
