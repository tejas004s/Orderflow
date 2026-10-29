import React, { useEffect, useState } from 'react';
import { api } from '../api';
import { formatPrice, formatUSD, type Instrument } from '../types';

interface OrderTicketProps {
  instrument: Instrument;
  draftPriceCents: number | null;
  cashAvailable: number;
  sharesAvailable: number;
  onOrderPlaced: () => void;
}

export const OrderTicket: React.FC<OrderTicketProps> = ({
  instrument,
  draftPriceCents,
  cashAvailable,
  sharesAvailable,
  onOrderPlaced,
}) => {
  const [side, setSide] = useState<'BUY' | 'SELL'>('BUY');
  const [orderType, setOrderType] = useState<'LIMIT' | 'MARKET'>('LIMIT');
  const [priceStr, setPriceStr] = useState<string>(formatPrice(instrument.last_price || instrument.ref_price));
  const [qtyStr, setQtyStr] = useState<string>('10');
  const [submitting, setSubmitting] = useState(false);
  const [feedback, setFeedback] = useState<{ kind: 'ok' | 'err'; text: string } | null>(null);

  useEffect(() => {
    setPriceStr(formatPrice(instrument.last_price || instrument.ref_price));
  }, [instrument.symbol]);

  useEffect(() => {
    if (draftPriceCents && draftPriceCents > 0) {
      setOrderType('LIMIT');
      setPriceStr(formatPrice(draftPriceCents));
    }
  }, [draftPriceCents]);

  const priceCents = Math.round(parseFloat(priceStr || '0') * 100);
  const qty = parseInt(qtyStr || '0', 10);
  const refPx = instrument.last_price || instrument.ref_price;
  const estPx =
    orderType === 'LIMIT'
      ? priceCents
      : side === 'BUY'
        ? Math.ceil((refPx * 105) / 100)
        : refPx;
  const estNotional = estPx > 0 && qty > 0 ? estPx * qty : 0;

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setFeedback(null);
    if (qty <= 0) {
      setFeedback({ kind: 'err', text: 'Quantity must be positive' });
      return;
    }
    if (orderType === 'LIMIT' && priceCents <= 0) {
      setFeedback({ kind: 'err', text: 'Limit price must be positive' });
      return;
    }
    setSubmitting(true);
    try {
      const ord = await api.placeOrder({
        symbol: instrument.symbol,
        side,
        type: orderType,
        price: orderType === 'LIMIT' ? priceCents : undefined,
        qty,
      });
      setFeedback({
        kind: 'ok',
        text: `${ord.side} ${ord.qty} ${ord.symbol} accepted (${ord.id.slice(0, 8)})`,
      });
      onOrderPlaced();
    } catch (err) {
      setFeedback({
        kind: 'err',
        text: err instanceof Error ? err.message : 'Order failed',
      });
    } finally {
      setSubmitting(false);
    }
  };

  const handleQuickSeed = async (kind: 'cash' | 'shares') => {
    try {
      if (kind === 'cash') {
        await api.seedPortfolio({ cash_cents: 10_000_000 });
        setFeedback({ kind: 'ok', text: 'Added +$100,000.00 virtual cash' });
      } else {
        await api.seedPortfolio({ symbol: instrument.symbol, shares: 100 });
        setFeedback({ kind: 'ok', text: `Granted +100 ${instrument.symbol} shares` });
      }
      onOrderPlaced();
    } catch (err) {
      setFeedback({
        kind: 'err',
        text: err instanceof Error ? err.message : 'Seed failed',
      });
    }
  };

  return (
    <div className="panel ticket-panel">
      <div className="panel-header">
        <span>Order Ticket · {instrument.symbol}</span>
        <span className="muted">GTC / IOC</span>
      </div>

      <div className="side-toggle">
        <button
          type="button"
          className={`side-btn buy-btn ${side === 'BUY' ? 'active' : ''}`}
          onClick={() => setSide('BUY')}
        >
          BUY
        </button>
        <button
          type="button"
          className={`side-btn sell-btn ${side === 'SELL' ? 'active' : ''}`}
          onClick={() => setSide('SELL')}
        >
          SELL
        </button>
      </div>

      <div className="type-toggle">
        <button
          type="button"
          className={`tab-btn ${orderType === 'LIMIT' ? 'active' : ''}`}
          onClick={() => setOrderType('LIMIT')}
        >
          Limit
        </button>
        <button
          type="button"
          className={`tab-btn ${orderType === 'MARKET' ? 'active' : ''}`}
          onClick={() => setOrderType('MARKET')}
        >
          Market
        </button>
      </div>

      <form onSubmit={handleSubmit} className="ticket-form">
        {orderType === 'LIMIT' ? (
          <label className="field">
            <span>Limit Price (USD)</span>
            <input
              type="number"
              step="0.01"
              min="0.01"
              value={priceStr}
              onChange={(e) => setPriceStr(e.target.value)}
              className="mono"
            />
          </label>
        ) : (
          <div className="market-band-note">
            Market {side}: executes immediately (IOC) with 5% price protection band.
          </div>
        )}

        <label className="field">
          <span>Quantity (Shares)</span>
          <input
            type="number"
            step={instrument.lot_size}
            min={instrument.lot_size}
            max={instrument.max_order_qty}
            value={qtyStr}
            onChange={(e) => setQtyStr(e.target.value)}
            className="mono"
          />
        </label>

        <div className="ticket-summary">
          <div className="summary-row">
            <span>Available {side === 'BUY' ? 'Cash' : instrument.symbol}</span>
            <span className="mono">
              {side === 'BUY' ? formatUSD(cashAvailable) : `${sharesAvailable.toLocaleString()} sh`}
            </span>
          </div>
          <div className="summary-row">
            <span>{side === 'BUY' ? 'Est. Reservation' : 'Est. Proceeds'}</span>
            <span className="mono">{formatUSD(estNotional)}</span>
          </div>
        </div>

        <button
          type="submit"
          disabled={submitting}
          className={`submit-order-btn ${side === 'BUY' ? 'buy-submit' : 'sell-submit'}`}
        >
          {submitting
            ? 'Submitting…'
            : `${side} ${qty > 0 ? qty : ''} ${instrument.symbol} @ ${
                orderType === 'MARKET' ? 'MKT' : `$${priceStr}`
              }`}
        </button>

        {feedback && (
          <div className={`ticket-feedback ${feedback.kind}`}>{feedback.text}</div>
        )}
      </form>

      <div className="seed-toolbar">
        <span className="muted">Paper Faucet:</span>
        <button type="button" className="ghost-btn" onClick={() => handleQuickSeed('cash')}>
          +$100k Cash
        </button>
        <button type="button" className="ghost-btn" onClick={() => handleQuickSeed('shares')}>
          +100 {instrument.symbol}
        </button>
      </div>
    </div>
  );
};
