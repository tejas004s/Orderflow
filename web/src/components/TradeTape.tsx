import React from 'react';
import { formatPrice, type Trade } from '../types';

interface TradeTapeProps {
  symbol: string;
  trades: Trade[];
}

function formatTradeTime(ts: string | number): string {
  if (typeof ts === 'number') {
    // unix nanoseconds -> ms
    const ms = ts > 1e14 ? Math.floor(ts / 1e6) : ts;
    return new Date(ms).toLocaleTimeString();
  }
  const d = new Date(ts);
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleTimeString();
}

export const TradeTape: React.FC<TradeTapeProps> = ({ symbol, trades }) => {
  return (
    <div className="panel tape-panel">
      <div className="panel-header">
        <span>Recent Trades · {symbol}</span>
        <span className="muted">Live Tape</span>
      </div>
      <div className="ob-cols-header">
        <span>Price (USD)</span>
        <span>Size</span>
        <span>Time</span>
      </div>
      <div className="tape-list">
        {trades.length === 0 ? (
          <div className="empty-row">Waiting for executions…</div>
        ) : (
          trades.slice(0, 35).map((tr, idx) => (
            <div key={tr.match_id || idx} className="ob-row">
              <span className={`mono ${tr.aggressor_side === 'BUY' ? 'bid-px' : 'ask-px'}`}>
                {formatPrice(tr.price)}
              </span>
              <span className="mono">{tr.qty.toLocaleString()}</span>
              <span className="mono muted">{formatTradeTime(tr.executed_at)}</span>
            </div>
          ))
        )}
      </div>
    </div>
  );
};
