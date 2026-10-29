// Single WebSocket client module feeding the Zustand marketStore with
// requestAnimationFrame batching, exponential-backoff reconnect, resubscribe,
// and automatic REST snapshot resync on sequence gaps (SDD sections 8.2, 9).
import { api, loadAuth } from '../api';
import { useMarketStore } from '../store/marketStore';
import type { BookState, Candle, Trade, WSOutboundMsg } from '../types';

class ExchangeWSClient {
  private ws: WebSocket | null = null;
  private symbol = 'AAPL';
  private reconnectTimer: number | null = null;
  private backoffMs = 500;
  private stopped = false;

  // Frame-batched pending deltas
  private rafId: number | null = null;
  private pendingBooks: Record<string, { state: BookState; prevSeq?: number; isSnapshot: boolean }> = {};
  private pendingTrades: Record<string, Trade[]> = {};
  private pendingPrices: Record<string, number> = {};
  private pendingCandles: Record<string, Candle> = {};
  private pendingOrderUpdates = 0;

  start(symbol: string): void {
    this.stopped = false;
    this.symbol = symbol;
    this.connect();
  }

  switchSymbol(nextSymbol: string): void {
    if (this.symbol === nextSymbol) return;
    const prev = this.symbol;
    this.symbol = nextSymbol;
    if (this.ws && this.ws.readyState === WebSocket.OPEN) {
      this.ws.send(
        JSON.stringify({
          op: 'unsubscribe',
          channels: [`book:${prev}`, `trades:${prev}`, `candles:${prev}`, `ticks:${prev}`],
        })
      );
      this.subscribeCurrent();
    }
  }

  stop(): void {
    this.stopped = true;
    if (this.reconnectTimer !== null) {
      window.clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (this.ws) {
      this.ws.close();
      this.ws = null;
    }
  }

  private connect(): void {
    if (this.stopped) return;
    const auth = loadAuth();
    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const q = auth?.access_token ? `?token=${encodeURIComponent(auth.access_token)}` : '';
    const url = `${proto}//${window.location.host}/ws${q}`;

    const sock = new WebSocket(url);
    this.ws = sock;

    sock.onopen = () => {
      this.backoffMs = 500;
      useMarketStore.getState().setConnected(true);
      this.subscribeCurrent();
    };

    sock.onmessage = (ev) => {
      try {
        const msg = JSON.parse(ev.data as string) as WSOutboundMsg;
        this.enqueueMessage(msg);
      } catch {
        // ignore malformed frame
      }
    };

    sock.onclose = () => {
      useMarketStore.getState().setConnected(false);
      if (!this.stopped) {
        const delay = this.backoffMs;
        this.backoffMs = Math.min(8000, this.backoffMs * 2);
        this.reconnectTimer = window.setTimeout(() => this.connect(), delay);
      }
    };
  }

  private subscribeCurrent(): void {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return;
    const sym = this.symbol;
    this.ws.send(
      JSON.stringify({
        op: 'subscribe',
        channels: [`book:${sym}`, `trades:${sym}`, `candles:${sym}`, `ticks:${sym}`, 'user.orders'],
      })
    );
  }

  private enqueueMessage(msg: WSOutboundMsg): void {
    switch (msg.type) {
      case 'book.snapshot': {
        const bk = msg.data as BookState;
        this.pendingBooks[bk.symbol] = { state: bk, isSnapshot: true };
        break;
      }
      case 'book.delta': {
        const bk = msg.data as BookState;
        this.pendingBooks[bk.symbol] = {
          state: bk,
          prevSeq: msg.prev_seq,
          isSnapshot: false,
        };
        break;
      }
      case 'trade': {
        const tr = msg.data as Trade;
        if (!this.pendingTrades[tr.symbol]) {
          this.pendingTrades[tr.symbol] = [];
        }
        this.pendingTrades[tr.symbol].unshift(tr);
        this.pendingPrices[tr.symbol] = tr.price;
        break;
      }
      case 'tick': {
        const tk = msg.data as { symbol: string; price: number };
        if (tk.price > 0) {
          this.pendingPrices[tk.symbol] = tk.price;
        }
        break;
      }
      case 'candle': {
        const cd = msg.data as Candle;
        this.pendingCandles[cd.symbol] = cd;
        this.pendingPrices[cd.symbol] = cd.close;
        break;
      }
      case 'order.update': {
        this.pendingOrderUpdates++;
        break;
      }
    }

    if (this.rafId === null) {
      this.rafId = window.requestAnimationFrame(() => this.flush());
    }
  }

  private flush(): void {
    this.rafId = null;
    const batch = {
      books: this.pendingBooks,
      trades: this.pendingTrades,
      prices: this.pendingPrices,
      candles: this.pendingCandles,
      orderUpdates: this.pendingOrderUpdates,
    };
    this.pendingBooks = {};
    this.pendingTrades = {};
    this.pendingPrices = {};
    this.pendingCandles = {};
    this.pendingOrderUpdates = 0;

    useMarketStore.getState().applyBatch(batch);

    // Check if any symbol flagged a sequence gap and resync via REST snapshot.
    const resync = useMarketStore.getState().resyncNeeded;
    for (const [sym, needed] of Object.entries(resync)) {
      if (needed) {
        useMarketStore.getState().clearResync(sym);
        api
          .getBook(sym, 20)
          .then((snap) => useMarketStore.getState().setBookSnapshot(snap))
          .catch(() => {});
      }
    }
  }
}

export const wsClient = new ExchangeWSClient();
