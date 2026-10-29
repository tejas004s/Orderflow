import { create } from 'zustand';
import type { BookState, Candle, Trade } from '../types';

interface MarketStore {
  connected: boolean;
  selectedSymbol: string;
  draftPriceCents: number | null;
  books: Record<string, BookState>;
  trades: Record<string, Trade[]>;
  lastPrices: Record<string, number>;
  latestCandle: Record<string, Candle>;
  orderEventTick: number;
  resyncNeeded: Record<string, boolean>;

  setConnected: (connected: boolean) => void;
  setSelectedSymbol: (symbol: string) => void;
  setDraftPriceCents: (price: number | null) => void;
  setBookSnapshot: (book: BookState) => void;
  setInitialTrades: (symbol: string, trades: Trade[]) => void;
  clearResync: (symbol: string) => void;
  applyBatch: (batch: {
    books: Record<string, { state: BookState; prevSeq?: number; isSnapshot: boolean }>;
    trades: Record<string, Trade[]>;
    prices: Record<string, number>;
    candles: Record<string, Candle>;
    orderUpdates: number;
  }) => void;
}

export const useMarketStore = create<MarketStore>((set) => ({
  connected: false,
  selectedSymbol: 'AAPL',
  draftPriceCents: null,
  books: {},
  trades: {},
  lastPrices: {},
  latestCandle: {},
  orderEventTick: 0,
  resyncNeeded: {},

  setConnected: (connected) => set({ connected }),
  setSelectedSymbol: (selectedSymbol) => set({ selectedSymbol, draftPriceCents: null }),
  setDraftPriceCents: (draftPriceCents) => set({ draftPriceCents }),

  setBookSnapshot: (book) =>
    set((s) => ({
      books: { ...s.books, [book.symbol]: book },
      resyncNeeded: { ...s.resyncNeeded, [book.symbol]: false },
    })),

  setInitialTrades: (symbol, list) =>
    set((s) => ({
      trades: { ...s.trades, [symbol]: list.slice(0, 60) },
    })),

  clearResync: (symbol) =>
    set((s) => ({
      resyncNeeded: { ...s.resyncNeeded, [symbol]: false },
    })),

  applyBatch: (batch) =>
    set((s) => {
      const nextBooks = { ...s.books };
      const nextResync = { ...s.resyncNeeded };

      for (const [sym, item] of Object.entries(batch.books)) {
        const cur = nextBooks[sym];
        if (!item.isSnapshot && cur && item.prevSeq !== undefined && item.prevSeq > cur.seq) {
          // Sequence gap detected (SDD section 8.2 & 9): flag symbol for REST snapshot resync.
          nextResync[sym] = true;
        }
        nextBooks[sym] = item.state;
      }

      const nextTrades = { ...s.trades };
      for (const [sym, incoming] of Object.entries(batch.trades)) {
        const existing = nextTrades[sym] || [];
        nextTrades[sym] = [...incoming, ...existing].slice(0, 60);
      }

      return {
        books: nextBooks,
        resyncNeeded: nextResync,
        trades: nextTrades,
        lastPrices: { ...s.lastPrices, ...batch.prices },
        latestCandle: { ...s.latestCandle, ...batch.candles },
        orderEventTick: s.orderEventTick + batch.orderUpdates,
      };
    }),
}));
