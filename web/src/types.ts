export interface Instrument {
  symbol: string;
  name: string;
  tick_size: number;
  lot_size: number;
  max_order_qty: number;
  ref_price: number;
  last_price: number;
  status: 'ACTIVE' | 'HALTED';
}

export interface Level {
  price: number;
  qty: number;
  orders: number;
}

export interface BookState {
  symbol: string;
  seq: number;
  bids: Level[];
  asks: Level[];
}

export interface Trade {
  id?: number;
  match_id: string;
  symbol: string;
  price: number;
  qty: number;
  aggressor_side: 'BUY' | 'SELL';
  executed_at: string | number;
}

export interface Candle {
  symbol: string;
  interval: string;
  bucket_start: number; // unix seconds
  open: number;
  high: number;
  low: number;
  close: number;
  volume: number;
}

export interface Order {
  id: string;
  user_id: string;
  symbol: string;
  side: 'BUY' | 'SELL';
  type: 'LIMIT' | 'MARKET';
  price?: number;
  protection_price?: number;
  qty: number;
  filled_qty: number;
  reserved_amount: number;
  status: 'NEW' | 'ACCEPTED' | 'PARTIALLY_FILLED' | 'FILLED' | 'CANCELLED' | 'REJECTED';
  reject_reason?: string;
  created_at: string;
  updated_at: string;
}

export interface Position {
  symbol: string;
  qty: number;
  reserved_qty: number;
  total_qty: number;
  avg_cost: number;
  mark_price: number;
  market_value: number;
  unrealised_pnl: number;
}

export interface Portfolio {
  cash_available: number;
  cash_reserved: number;
  cash_total: number;
  positions: Position[];
  total_market_value: number;
  total_unrealised_pnl: number;
  total_equity: number;
}

export interface TokenPair {
  user_id: string;
  email: string;
  access_token: string;
  refresh_token: string;
  expires_in: number;
}

export interface WSOutboundMsg {
  type: 'book.snapshot' | 'book.delta' | 'trade' | 'tick' | 'candle' | 'order.update';
  channel?: string;
  symbol?: string;
  seq?: number;
  prev_seq?: number;
  data: unknown;
}

export function formatUSD(cents: number): string {
  const sign = cents < 0 ? '-' : '';
  const abs = Math.abs(cents) / 100;
  return `${sign}$${abs.toLocaleString('en-US', {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  })}`;
}

export function formatPrice(cents: number): string {
  return (cents / 100).toFixed(2);
}
