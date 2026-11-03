import React, { useEffect, useState } from 'react';
import { api } from '../api';
import { formatPrice, formatUSD, type Instrument, type OrderType } from '../types';

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
  const [orderType, setOrderType] = useState<OrderType>('LIMIT');
  const [priceStr, setPriceStr] = useState<string>(
    formatPrice(instrument.last_price || instrument.ref_price)
  );
  const [stopPriceStr, setStopPriceStr] = useState<string>(
    formatPrice(instrument.last_price || instrument.ref_price)
  );
  const [qtyStr, setQtyStr] = useState<string>('10');
  const [submitting, setSubmitting] = useState(false);
  const [feedback, setFeedback] = useState<{ kind: 'ok' | 'err'; text: string } | null>(null);

  useEffect(() => {
    const px = formatPrice(instrument.last_price || instrument.ref_price);
    setPriceStr(px);
    setStopPriceStr(px);
  }, [instrument.symbol]);

  useEffect(() => {
    if (draftPriceCents && draftPriceCents > 0) {
      if (orderType === 'MARKET' || orderType === 'STOP') {
        setOrderType('LIMIT');
      }
      setPriceStr(formatPrice(draftPriceCents));
    }
  }, [draftPriceCents]);

  const priceCents = Math.round(parseFloat(priceStr || '0') * 100);
  const stopPriceCents = Math.round(parseFloat(stopPriceStr || '0') * 100);
  const qty = parseInt(qtyStr || '0', 10);
  const refPx = instrument.last_price || instrument.ref_price;
  const estPx =
    orderType === 'LIMIT' || orderType === 'STOP_LIMIT'
      ? priceCents
      : orderType === 'STOP'
        ? side === 'BUY'
          ? Math.ceil((stopPriceCents * 105) / 100)
          : stopPriceCents
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
    if ((orderType === 'LIMIT' || orderType === 'STOP_LIMIT') && priceCents <= 0) {
      setFeedback({ kind: 'err', text: 'Limit price must be positive' });
      return;
    }
    if ((orderType === 'STOP' || orderType === 'STOP_LIMIT') && stopPriceCents <= 0) {
      setFeedback({ kind: 'err', text: 'Stop price must be positive' });
      return;
    }
    setSubmitting(true);
    try {
      const ord = await api.placeOrder({
        symbol: instrument.symbol,
        side,
        type: orderType,
        price: orderType === 'LIMIT' || orderType === 'STOP_LIMIT' ? priceCents : undefined,
        stop_price: orderType === 'STOP' || orderType === 'STOP_LIMIT' ? stopPriceCents : undefined,
        qty,
      });
      setFeedback({
        kind: 'ok',
        text: `${ord.side} ${ord.qty} ${ord.symbol} order placed (${ord.id.slice(0, 8)})`,
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
        setFeedback({ kind: 'ok', text: `Credited +100 ${instrument.symbol} shares` });
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
    <div className="groww-card groww-order-pad">
      {/* DMA Order Pad Header */}
      <div className="groww-pad-top">
        <div className="groww-pad-stock-info">
          <div className="groww-pad-title">DMA Order Entry · {instrument.symbol}</div>
          <div className="groww-pad-exch">
            OFX · EQ · {instrument.name} · LTP <span className="mono">${formatPrice(refPx)}</span>
          </div>
        </div>
      </div>

      {/* Groww BUY / SELL Underline Tabs */}
      <div className="groww-pad-side-tabs">
        <button
          type="button"
          className={`groww-side-tab buy-tab ${side === 'BUY' ? 'active' : ''}`}
          onClick={() => setSide('BUY')}
        >
          BUY
        </button>
        <button
          type="button"
          className={`groww-side-tab sell-tab ${side === 'SELL' ? 'active' : ''}`}
          onClick={() => setSide('SELL')}
        >
          SELL
        </button>
      </div>

      {/* Groww Order Type Pill Selector */}
      <div className="groww-pad-pills">
        <button
          type="button"
          className={`groww-type-pill ${orderType === 'LIMIT' ? 'active' : ''}`}
          onClick={() => setOrderType('LIMIT')}
        >
          Limit
        </button>
        <button
          type="button"
          className={`groww-type-pill ${orderType === 'MARKET' ? 'active' : ''}`}
          onClick={() => setOrderType('MARKET')}
        >
          Market
        </button>
        <button
          type="button"
          className={`groww-type-pill ${orderType === 'STOP' ? 'active' : ''}`}
          onClick={() => setOrderType('STOP')}
        >
          SL-M (Stop)
        </button>
        <button
          type="button"
          className={`groww-type-pill ${orderType === 'STOP_LIMIT' ? 'active' : ''}`}
          onClick={() => setOrderType('STOP_LIMIT')}
        >
          SL-L (Stop Limit)
        </button>
      </div>

      <form onSubmit={handleSubmit} className="groww-pad-form">
        {/* Qty Row */}
        <div className="groww-input-row">
          <div className="groww-input-label">
            <span>Order Qty</span>
            <span className="groww-input-sub">OFX · Lot Size {instrument.lot_size}</span>
          </div>
          <div className="groww-input-box-wrap">
            <input
              type="number"
              step={instrument.lot_size}
              min={instrument.lot_size}
              max={instrument.max_order_qty}
              value={qtyStr}
              onChange={(e) => setQtyStr(e.target.value)}
              className="groww-pad-input mono"
            />
          </div>
        </div>

        {/* Quick Qty Chips */}
        <div className="groww-quick-qty">
          {[5, 10, 25, 50, 100].map((q) => (
            <button
              key={q}
              type="button"
              className={`groww-qty-chip ${qty === q ? 'active' : ''}`}
              onClick={() => setQtyStr(String(q))}
            >
              {q} sh
            </button>
          ))}
        </div>

        {/* Stop Trigger Price Row (for STOP and STOP_LIMIT) */}
        {(orderType === 'STOP' || orderType === 'STOP_LIMIT') && (
          <div className="groww-input-row">
            <div className="groww-input-label">
              <span>Stop Price (USD)</span>
              <span className="groww-input-sub">Trigger at $</span>
            </div>
            <div className="groww-input-box-wrap">
              <span className="groww-currency-prefix">$</span>
              <input
                type="number"
                step="0.01"
                min="0.01"
                value={stopPriceStr}
                onChange={(e) => setStopPriceStr(e.target.value)}
                className="groww-pad-input mono with-prefix"
              />
            </div>
          </div>
        )}

        {/* Price Row */}
        {orderType === 'LIMIT' || orderType === 'STOP_LIMIT' ? (
          <div className="groww-input-row">
            <div className="groww-input-label">
              <span>Price</span>
              <span className="groww-input-sub">Limit Order</span>
            </div>
            <div className="groww-input-box-wrap">
              <span className="groww-currency-prefix">$</span>
              <input
                type="number"
                step="0.01"
                min="0.01"
                value={priceStr}
                onChange={(e) => setPriceStr(e.target.value)}
                className="groww-pad-input mono with-prefix"
              />
            </div>
          </div>
        ) : (
          <div className="groww-input-row">
            <div className="groww-input-label">
              <span>Price</span>
              <span className="groww-input-sub">
                {orderType === 'STOP' ? 'Market on Trigger' : 'Instant IOC (5% band)'}
              </span>
            </div>
            <div className="groww-input-box-wrap">
              <input
                type="text"
                disabled
                value="At Market"
                className="groww-pad-input disabled-market"
              />
            </div>
          </div>
        )}

        <div className="groww-pad-divider" />

        {/* Balance & Margin Summary */}
        <div className="groww-pad-summary">
          <div className="groww-summary-line">
            <span className="muted">
              {side === 'BUY' ? 'Balance Available' : `${instrument.symbol} Available`}
            </span>
            <span className="mono bold">
              {side === 'BUY'
                ? formatUSD(cashAvailable)
                : `${sharesAvailable.toLocaleString()} shares`}
            </span>
          </div>
          <div className="groww-summary-line">
            <span className="muted">{side === 'BUY' ? 'Approx. Req.' : 'Approx. Value'}</span>
            <span className="mono bold">{formatUSD(estNotional)}</span>
          </div>
          <div className="groww-summary-line fee-line">
            <span className="muted">Exchange Fee Tier</span>
            <span className="mono muted">Maker -1 bps · Taker +5 bps</span>
          </div>
        </div>

        <button
          type="submit"
          disabled={submitting || instrument.status === 'HALTED'}
          className={`groww-submit-btn ${side === 'BUY' ? 'groww-buy-btn' : 'groww-sell-btn'}`}
        >
          {instrument.status === 'HALTED'
            ? `TRADING HALTED (LULD) · ${instrument.symbol}`
            : submitting
              ? 'Placing Order…'
              : `${side} ${instrument.symbol}`}
        </button>

        {feedback && (
          <div className={`groww-feedback ${feedback.kind === 'ok' ? 'ok' : 'err'}`}>
            {feedback.text}
          </div>
        )}
      </form>

      {/* Paper Trading Faucet Footer */}
      <div className="groww-faucet-footer">
        <span className="muted">Paper Wallet Faucet</span>
        <div className="groww-faucet-btns">
          <button
            type="button"
            className="groww-faucet-btn"
            onClick={() => handleQuickSeed('cash')}
          >
            + $100k Cash
          </button>
          <button
            type="button"
            className="groww-faucet-btn"
            onClick={() => handleQuickSeed('shares')}
          >
            + 100 {instrument.symbol}
          </button>
        </div>
      </div>
    </div>
  );
};
