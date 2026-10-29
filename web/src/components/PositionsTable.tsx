import React from 'react';
import { formatPrice, formatUSD, type Order, type Position } from '../types';

interface PositionsTableProps {
  positions: Position[];
  orders: Order[];
  onCancelOrder: (orderId: string) => void;
  onSelectSymbol: (symbol: string) => void;
}

export const PositionsTable: React.FC<PositionsTableProps> = ({
  positions,
  orders,
  onCancelOrder,
  onSelectSymbol,
}) => {
  const [tab, setTab] = React.useState<'positions' | 'open_orders' | 'history'>('positions');

  const openOrders = orders.filter(
    (o) => o.status === 'NEW' || o.status === 'ACCEPTED' || o.status === 'PARTIALLY_FILLED'
  );

  return (
    <div className="panel bottom-panel">
      <div className="bottom-tabs">
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
                        {o.price ? `$${formatPrice(o.price)}` : 'MKT'}
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
    </div>
  );
};
