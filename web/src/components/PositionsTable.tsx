import React from 'react';
import { api } from '../api';
import {
  formatPrice,
  formatUSD,
  type LeaderboardEntry,
  type Order,
  type Position,
} from '../types';

export interface EquityPoint {
  ts: number;
  equity: number;
}

interface PositionsTableProps {
  positions: Position[];
  orders: Order[];
  leaderboard?: LeaderboardEntry[];
  equityHistory?: EquityPoint[];
  currentUserId?: string;
  onCancelOrder: (orderId: string) => void;
  onSelectSymbol: (symbol: string) => void;
}

export const PositionsTable: React.FC<PositionsTableProps> = ({
  positions,
  orders,
  leaderboard = [],
  equityHistory = [],
  currentUserId,
  onCancelOrder,
  onSelectSymbol,
}) => {
  const [tab, setTab] = React.useState<
    'positions' | 'open_orders' | 'history' | 'leaderboard' | 'equity'
  >('positions');
  const [exporting, setExporting] = React.useState(false);

  const openOrders = orders.filter(
    (o) => o.status === 'NEW' || o.status === 'ACCEPTED' || o.status === 'PARTIALLY_FILLED'
  );

  const handleDownloadCSV = async () => {
    try {
      setExporting(true);
      await api.downloadFillsCSV();
    } catch {
      // ignore
    } finally {
      setExporting(false);
    }
  };

  return (
    <div className="panel bottom-panel">
      <div
        className="bottom-tabs"
        style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}
      >
        <div style={{ display: 'flex', gap: '4px', flexWrap: 'wrap' }}>
          <button
            type="button"
            className={`tab-btn ${tab === 'positions' ? 'active' : ''}`}
            onClick={() => setTab('positions')}
          >
            Positions ({positions.length})
          </button>
          <button
            type="button"
            className={`tab-btn ${tab === 'open_orders' ? 'active' : ''}`}
            onClick={() => setTab('open_orders')}
          >
            Open Orders ({openOrders.length})
          </button>
          <button
            type="button"
            className={`tab-btn ${tab === 'history' ? 'active' : ''}`}
            onClick={() => setTab('history')}
          >
            Order History ({orders.length})
          </button>
          <button
            type="button"
            className={`tab-btn ${tab === 'leaderboard' ? 'active' : ''}`}
            onClick={() => setTab('leaderboard')}
          >
            Leaderboard ({leaderboard.length})
          </button>
          <button
            type="button"
            className={`tab-btn ${tab === 'equity' ? 'active' : ''}`}
            onClick={() => setTab('equity')}
          >
            Equity Curve
          </button>
        </div>

        <button
          type="button"
          className="ghost-btn"
          style={{ fontSize: '11px', padding: '4px 10px', marginRight: '8px' }}
          disabled={exporting}
          onClick={handleDownloadCSV}
        >
          {exporting ? 'Exporting…' : '⬇ Download Fill History (CSV)'}
        </button>
      </div>

      {tab === 'positions' && (
        <div className="table-wrap">
          <table className="data-table">
            <thead>
              <tr>
                <th>Symbol</th>
                <th>Total Shares</th>
                <th>Avail / Reserved</th>
                <th>Avg Cost</th>
                <th>Mark Price</th>
                <th>Market Value</th>
                <th>Unrealised P&amp;L</th>
              </tr>
            </thead>
            <tbody>
              {positions.length === 0 ? (
                <tr>
                  <td colSpan={7} className="empty-cell">
                    No open positions. Place a BUY order or use the Paper Faucet.
                  </td>
                </tr>
              ) : (
                positions.map((p) => (
                  <tr key={p.symbol} onClick={() => onSelectSymbol(p.symbol)} className="clickable-row">
                    <td className="mono bold">{p.symbol}</td>
                    <td className="mono">{p.total_qty.toLocaleString()}</td>
                    <td className="mono muted">
                      {p.qty.toLocaleString()} / {p.reserved_qty.toLocaleString()}
                    </td>
                    <td className="mono">${formatPrice(p.avg_cost)}</td>
                    <td className="mono">${formatPrice(p.mark_price)}</td>
                    <td className="mono">{formatUSD(p.market_value)}</td>
                    <td
                      className={`mono bold ${
                        p.unrealised_pnl >= 0 ? 'pnl-pos' : 'pnl-neg'
                      }`}
                    >
                      {p.unrealised_pnl >= 0 ? '+' : ''}
                      {formatUSD(p.unrealised_pnl)}
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      )}

      {(tab === 'open_orders' || tab === 'history') && (
        <div className="table-wrap">
          <table className="data-table">
            <thead>
              <tr>
                <th>Order ID</th>
                <th>Symbol</th>
                <th>Side</th>
                <th>Type</th>
                <th>Price</th>
                <th>Filled / Qty</th>
                <th>Status</th>
                <th>Created</th>
                <th>Action</th>
              </tr>
            </thead>
            <tbody>
              {(tab === 'open_orders' ? openOrders : orders).length === 0 ? (
                <tr>
                  <td colSpan={9} className="empty-cell">
                    No orders to display.
                  </td>
                </tr>
              ) : (
                (tab === 'open_orders' ? openOrders : orders).map((o) => {
                  const canCancel =
                    o.status === 'NEW' ||
                    o.status === 'ACCEPTED' ||
                    o.status === 'PARTIALLY_FILLED';
                  return (
                    <tr key={o.id}>
                      <td className="mono muted">{o.id.slice(0, 8)}</td>
                      <td className="mono bold">{o.symbol}</td>
                      <td>
                        <span className={`badge ${o.side === 'BUY' ? 'badge-buy' : 'badge-sell'}`}>
                          {o.side}
                        </span>
                      </td>
                      <td className="mono">{o.type}</td>
                      <td className="mono">
                        {o.type === 'STOP' && o.stop_price
                          ? `Stop $${formatPrice(o.stop_price)}`
                          : o.type === 'STOP_LIMIT' && o.stop_price && o.price
                            ? `$${formatPrice(o.price)} (Stop $${formatPrice(o.stop_price)})`
                            : o.price
                              ? `$${formatPrice(o.price)}`
                              : 'MKT'}
                      </td>
                      <td className="mono">
                        {o.filled_qty} / {o.qty}
                      </td>
                      <td>
                        <span className={`status-pill status-${o.status.toLowerCase()}`}>
                          {o.status}
                        </span>
                      </td>
                      <td className="mono muted">
                        {new Date(o.created_at).toLocaleTimeString()}
                      </td>
                      <td>
                        {canCancel && (
                          <button
                            type="button"
                            className="cancel-btn"
                            onClick={() => onCancelOrder(o.id)}
                          >
                            Cancel
                          </button>
                        )}
                      </td>
                    </tr>
                  );
                })
              )}
            </tbody>
          </table>
        </div>
      )}

      {tab === 'leaderboard' && (
        <div className="table-wrap">
          <table className="data-table">
            <thead>
              <tr>
                <th>Rank</th>
                <th>Trader</th>
                <th>Total Equity</th>
                <th>Cash Balance</th>
                <th>Positions Value</th>
                <th>Unrealised P&amp;L</th>
                <th>Fills</th>
              </tr>
            </thead>
            <tbody>
              {leaderboard.length === 0 ? (
                <tr>
                  <td colSpan={7} className="empty-cell">
                    No leaderboard entries available.
                  </td>
                </tr>
              ) : (
                leaderboard.map((row) => {
                  const isMe = currentUserId && row.user_id === currentUserId;
                  return (
                    <tr
                      key={row.user_id}
                      style={isMe ? { background: 'rgba(59, 130, 246, 0.12)' } : undefined}
                    >
                      <td className="mono bold">#{row.rank}</td>
                      <td className="mono bold">
                        {row.trader} {isMe ? '(You)' : ''}
                      </td>
                      <td className="mono bold">{formatUSD(row.total_equity)}</td>
                      <td className="mono muted">{formatUSD(row.cash_total)}</td>
                      <td className="mono">{formatUSD(row.market_value)}</td>
                      <td
                        className={`mono bold ${
                          row.unrealised_pnl >= 0 ? 'pnl-pos' : 'pnl-neg'
                        }`}
                      >
                        {row.unrealised_pnl >= 0 ? '+' : ''}
                        {formatUSD(row.unrealised_pnl)}
                      </td>
                      <td className="mono">{row.trade_count.toLocaleString()}</td>
                    </tr>
                  );
                })
              )}
            </tbody>
          </table>
        </div>
      )}

      {tab === 'equity' && (
        <div className="table-wrap" style={{ padding: '12px 16px' }}>
          {equityHistory.length < 2 ? (
            <div className="empty-cell">
              Collecting live portfolio equity samples… ({equityHistory.length} point
              {equityHistory.length === 1 ? '' : 's'})
            </div>
          ) : (
            (() => {
              const w = 720;
              const h = 110;
              const vals = equityHistory.map((p) => p.equity);
              const minV = Math.min(...vals);
              const maxV = Math.max(...vals);
              const span = Math.max(100, maxV - minV);
              const pts = equityHistory
                .map((p, idx) => {
                  const x = (idx / (equityHistory.length - 1)) * (w - 20) + 10;
                  const y = h - 14 - ((p.equity - minV) / span) * (h - 28);
                  return `${x.toFixed(1)},${y.toFixed(1)}`;
                })
                .join(' ');
              const first = equityHistory[0].equity;
              const last = equityHistory[equityHistory.length - 1].equity;
              const delta = last - first;
              const stroke = delta >= 0 ? '#10b981' : '#ef4444';
              return (
                <div>
                  <div
                    style={{
                      display: 'flex',
                      justifyContent: 'space-between',
                      marginBottom: '6px',
                      fontSize: '12px',
                    }}
                  >
                    <span className="mono muted">
                      Session Start: <strong>{formatUSD(first)}</strong>
                    </span>
                    <span className="mono">
                      Current Equity: <strong>{formatUSD(last)}</strong> (
                      <span className={delta >= 0 ? 'pnl-pos' : 'pnl-neg'}>
                        {delta >= 0 ? '+' : ''}
                        {formatUSD(delta)}
                      </span>
                      )
                    </span>
                  </div>
                  <svg
                    viewBox={`0 0 ${w} ${h}`}
                    style={{
                      width: '100%',
                      height: '110px',
                      background: 'rgba(15, 23, 42, 0.55)',
                      borderRadius: '6px',
                      border: '1px solid #1e293b',
                    }}
                  >
                    <polyline
                      fill="none"
                      stroke={stroke}
                      strokeWidth="2.2"
                      points={pts}
                    />
                  </svg>
                </div>
              );
            })()
          )}
        </div>
      )}
    </div>
  );
};
