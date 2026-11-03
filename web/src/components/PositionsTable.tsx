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

export type BottomTab = 'positions' | 'open_orders' | 'history' | 'leaderboard' | 'equity';

interface PositionsTableProps {
  positions: Position[];
  orders: Order[];
  leaderboard?: LeaderboardEntry[];
  equityHistory?: EquityPoint[];
  currentUserId?: string;
  activeTab?: BottomTab;
  onChangeTab?: (tab: BottomTab) => void;
  onCancelOrder: (orderId: string) => void;
  onOrderAmended?: () => void;
  onSelectSymbol: (symbol: string) => void;
}

export const PositionsTable: React.FC<PositionsTableProps> = ({
  positions,
  orders,
  leaderboard = [],
  equityHistory = [],
  currentUserId,
  activeTab,
  onChangeTab,
  onCancelOrder,
  onOrderAmended,
  onSelectSymbol,
}) => {
  const [internalTab, setInternalTab] = React.useState<BottomTab>('positions');
  const tab = activeTab ?? internalTab;
  const setTab = (next: BottomTab) => {
    setInternalTab(next);
    onChangeTab?.(next);
  };

  const [exporting, setExporting] = React.useState(false);
  const [editingOrderId, setEditingOrderId] = React.useState<string | null>(null);
  const [editPriceStr, setEditPriceStr] = React.useState<string>('');
  const [editQtyStr, setEditQtyStr] = React.useState<string>('');
  const [amendBusy, setAmendBusy] = React.useState(false);

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

  const startModify = (o: Order) => {
    setEditingOrderId(o.id);
    setEditPriceStr(o.price ? formatPrice(o.price) : '');
    setEditQtyStr(String(o.qty));
  };

  const submitModify = async (orderId: string) => {
    const newPx = Math.round(parseFloat(editPriceStr || '0') * 100);
    const newQty = parseInt(editQtyStr || '0', 10);
    if (newPx <= 0 || newQty <= 0) return;
    try {
      setAmendBusy(true);
      await api.replaceOrder(orderId, { price: newPx, qty: newQty });
      setEditingOrderId(null);
      onOrderAmended?.();
    } catch {
      // ignore
    } finally {
      setAmendBusy(false);
    }
  };

  return (
    <div className="groww-card bottom-panel">
      <div className="groww-tabs-header">
        <div className="groww-underline-tabs">
          <button
            type="button"
            className={`groww-tab-item ${tab === 'positions' ? 'active' : ''}`}
            onClick={() => setTab('positions')}
          >
            Net Positions ({positions.length})
          </button>
          <button
            type="button"
            className={`groww-tab-item ${tab === 'open_orders' ? 'active' : ''}`}
            onClick={() => setTab('open_orders')}
          >
            Working Orders ({openOrders.length})
          </button>
          <button
            type="button"
            className={`groww-tab-item ${tab === 'history' ? 'active' : ''}`}
            onClick={() => setTab('history')}
          >
            Order Blotter ({orders.length})
          </button>
          <button
            type="button"
            className={`groww-tab-item ${tab === 'leaderboard' ? 'active' : ''}`}
            onClick={() => setTab('leaderboard')}
          >
            Member Leaderboard ({leaderboard.length})
          </button>
          <button
            type="button"
            className={`groww-tab-item ${tab === 'equity' ? 'active' : ''}`}
            onClick={() => setTab('equity')}
          >
            Clearing Equity Curve
          </button>
        </div>

        <button
          type="button"
          className="groww-csv-btn"
          disabled={exporting}
          onClick={handleDownloadCSV}
        >
          {exporting ? 'Exporting…' : '⬇ Export Trade Register (CSV)'}
        </button>
      </div>

      {tab === 'positions' && (
        <div className="table-wrap">
          <table className="groww-table">
            <thead>
              <tr>
                <th>Company</th>
                <th>Total Shares</th>
                <th>Avail / Reserved</th>
                <th>Avg. Buy Price</th>
                <th>Market Price (LTP)</th>
                <th>Current Value</th>
                <th className="text-right">Returns (P&amp;L)</th>
              </tr>
            </thead>
            <tbody>
              {positions.length === 0 ? (
                <tr>
                  <td colSpan={7} className="empty-cell">
                    You have no holdings yet. Place a BUY order or use the Paper Wallet Faucet on the right.
                  </td>
                </tr>
              ) : (
                positions.map((p) => {
                  const costBasis = p.total_qty * p.avg_cost;
                  const pctReturn =
                    costBasis > 0 ? ((p.unrealised_pnl / costBasis) * 100).toFixed(2) : '0.00';
                  return (
                    <tr
                      key={p.symbol}
                      onClick={() => onSelectSymbol(p.symbol)}
                      className="clickable-row"
                    >
                      <td>
                        <div className="groww-company-cell">
                          <span className="groww-mini-logo">{p.symbol.slice(0, 2)}</span>
                          <span className="bold">{p.symbol}</span>
                        </div>
                      </td>
                      <td className="mono bold">{p.total_qty.toLocaleString()}</td>
                      <td className="mono muted">
                        {p.qty.toLocaleString()} / {p.reserved_qty.toLocaleString()}
                      </td>
                      <td className="mono">${formatPrice(p.avg_cost)}</td>
                      <td className="mono bold">${formatPrice(p.mark_price)}</td>
                      <td className="mono bold">{formatUSD(p.market_value)}</td>
                      <td
                        className={`mono bold text-right ${
                          p.unrealised_pnl >= 0 ? 'pnl-pos' : 'pnl-neg'
                        }`}
                      >
                        {p.unrealised_pnl >= 0 ? '+' : ''}
                        {formatUSD(p.unrealised_pnl)} ({p.unrealised_pnl >= 0 ? '+' : ''}
                        {pctReturn}%)
                      </td>
                    </tr>
                  );
                })
              )}
            </tbody>
          </table>
        </div>
      )}

      {(tab === 'open_orders' || tab === 'history') && (
        <div className="table-wrap">
          <table className="groww-table">
            <thead>
              <tr>
                <th>Order ID</th>
                <th>Stock</th>
                <th>Side</th>
                <th>Type</th>
                <th>Order Price</th>
                <th>Filled / Qty</th>
                <th>Status</th>
                <th>Time</th>
                <th className="text-right">Action</th>
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
                  const canModify = canCancel && o.type === 'LIMIT';
                  const isEditing = editingOrderId === o.id;

                  return (
                    <tr key={o.id}>
                      <td className="mono muted">{o.id.slice(0, 8)}</td>
                      <td>
                        <span
                          className="bold clickable-sym"
                          onClick={() => onSelectSymbol(o.symbol)}
                        >
                          {o.symbol}
                        </span>
                      </td>
                      <td>
                        <span
                          className={`groww-side-pill ${
                            o.side === 'BUY' ? 'pill-buy' : 'pill-sell'
                          }`}
                        >
                          {o.side}
                        </span>
                      </td>
                      <td className="mono">{o.type}</td>
                      <td className="mono">
                        {isEditing ? (
                          <input
                            type="number"
                            step="0.01"
                            value={editPriceStr}
                            onChange={(e) => setEditPriceStr(e.target.value)}
                            className="groww-inline-input mono"
                          />
                        ) : o.type === 'STOP' && o.stop_price ? (
                          `Stop $${formatPrice(o.stop_price)}`
                        ) : o.type === 'STOP_LIMIT' && o.stop_price && o.price ? (
                          `$${formatPrice(o.price)} (SL $${formatPrice(o.stop_price)})`
                        ) : o.price ? (
                          `$${formatPrice(o.price)}`
                        ) : (
                          'Market'
                        )}
                      </td>
                      <td className="mono">
                        {isEditing ? (
                          <input
                            type="number"
                            step="1"
                            value={editQtyStr}
                            onChange={(e) => setEditQtyStr(e.target.value)}
                            className="groww-inline-input mono"
                          />
                        ) : (
                          `${o.filled_qty} / ${o.qty}`
                        )}
                      </td>
                      <td>
                        <span className={`groww-status-badge status-${o.status.toLowerCase()}`}>
                          {o.status}
                        </span>
                      </td>
                      <td className="mono muted">
                        {new Date(o.created_at).toLocaleTimeString()}
                      </td>
                      <td className="text-right">
                        {isEditing ? (
                          <div className="groww-action-group">
                            <button
                              type="button"
                              className="groww-modify-save"
                              disabled={amendBusy}
                              onClick={() => submitModify(o.id)}
                            >
                              Save
                            </button>
                            <button
                              type="button"
                              className="groww-modify-cancel"
                              onClick={() => setEditingOrderId(null)}
                            >
                              ✕
                            </button>
                          </div>
                        ) : (
                          <div className="groww-action-group">
                            {canModify && (
                              <button
                                type="button"
                                className="groww-modify-btn"
                                onClick={() => startModify(o)}
                              >
                                Modify
                              </button>
                            )}
                            {canCancel && (
                              <button
                                type="button"
                                className="groww-cancel-btn"
                                onClick={() => onCancelOrder(o.id)}
                              >
                                Cancel
                              </button>
                            )}
                          </div>
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
          <table className="groww-table">
            <thead>
              <tr>
                <th>Rank</th>
                <th>Investor</th>
                <th>Total Portfolio Value</th>
                <th>Cash Balance</th>
                <th>Holdings Value</th>
                <th>Unrealised Returns</th>
                <th className="text-right">Total Trades</th>
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
                      className={isMe ? 'groww-highlight-row' : undefined}
                    >
                      <td className="mono bold">#{row.rank}</td>
                      <td>
                        <div className="groww-company-cell">
                          <span className="groww-mini-logo">
                            {row.trader.slice(0, 2).toUpperCase()}
                          </span>
                          <span className="bold">
                            {row.trader} {isMe ? ' (You)' : ''}
                          </span>
                        </div>
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
                      <td className="mono text-right">{row.trade_count.toLocaleString()}</td>
                    </tr>
                  );
                })
              )}
            </tbody>
          </table>
        </div>
      )}

      {tab === 'equity' && (
        <div className="groww-equity-box">
          {equityHistory.length < 2 ? (
            <div className="empty-cell">
              Collecting live portfolio equity curve points… ({equityHistory.length} sample
              {equityHistory.length === 1 ? '' : 's'})
            </div>
          ) : (
            (() => {
              const w = 760;
              const h = 140;
              const vals = equityHistory.map((p) => p.equity);
              const minV = Math.min(...vals);
              const maxV = Math.max(...vals);
              const span = Math.max(100, maxV - minV);
              const pts = equityHistory
                .map((p, idx) => {
                  const x = (idx / (equityHistory.length - 1)) * (w - 24) + 12;
                  const y = h - 18 - ((p.equity - minV) / span) * (h - 36);
                  return `${x.toFixed(1)},${y.toFixed(1)}`;
                })
                .join(' ');
              const first = equityHistory[0].equity;
              const last = equityHistory[equityHistory.length - 1].equity;
              const delta = last - first;
              const stroke = delta >= 0 ? '#00b386' : '#eb5b3c';
              return (
                <div>
                  <div className="groww-equity-stats">
                    <span>
                      Starting Value: <strong className="mono">{formatUSD(first)}</strong>
                    </span>
                    <span>
                      Current Net Worth: <strong className="mono">{formatUSD(last)}</strong> (
                      <span className={`mono bold ${delta >= 0 ? 'pnl-pos' : 'pnl-neg'}`}>
                        {delta >= 0 ? '+' : ''}
                        {formatUSD(delta)}
                      </span>
                      )
                    </span>
                  </div>
                  <svg viewBox={`0 0 ${w} ${h}`} className="groww-equity-svg">
                    <polyline
                      fill="none"
                      stroke={stroke}
                      strokeWidth="2.5"
                      strokeLinecap="round"
                      strokeLinejoin="round"
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
