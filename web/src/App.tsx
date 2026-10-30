import React, { useEffect, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api, loadAuth, saveAuth } from './api';
import { OrderBook } from './components/OrderBook';
import { OrderTicket } from './components/OrderTicket';
import { PositionsTable, type EquityPoint } from './components/PositionsTable';
import { PriceChart, type CandleInterval } from './components/PriceChart';
import { TradeTape } from './components/TradeTape';
import { useMarketStore } from './store/marketStore';
import { formatPrice, formatUSD, type Instrument, type TokenPair } from './types';
import { wsClient } from './ws/client';

const DEFAULT_INSTRUMENT: Instrument = {
  symbol: 'AAPL',
  name: 'Apple Inc.',
  tick_size: 1,
  lot_size: 1,
  max_order_qty: 100000,
  ref_price: 18900,
  last_price: 18900,
  status: 'ACTIVE',
};

export const App: React.FC = () => {
  const [auth, setAuth] = useState<TokenPair | null>(() => loadAuth());
  const [authMode, setAuthMode] = useState<'login' | 'register'>('register');
  const [email, setEmail] = useState('trader@orderflow.dev');
  const [password, setPassword] = useState('trader123');
  const [authError, setAuthError] = useState<string | null>(null);
  const [candleInterval, setCandleInterval] = useState<CandleInterval>('1m');
  const [equityHistory, setEquityHistory] = useState<EquityPoint[]>([]);
  const [nowMs, setNowMs] = useState<number>(() => Date.now());

  const queryClient = useQueryClient();
  const connected = useMarketStore((s) => s.connected);
  const selectedSymbol = useMarketStore((s) => s.selectedSymbol);
  const draftPriceCents = useMarketStore((s) => s.draftPriceCents);
  const books = useMarketStore((s) => s.books);
  const tradesMap = useMarketStore((s) => s.trades);
  const lastPrices = useMarketStore((s) => s.lastPrices);
  const latestCandle = useMarketStore((s) => s.latestCandle);
  const haltedUntilMap = useMarketStore((s) => s.haltedUntil);
  const orderEventTick = useMarketStore((s) => s.orderEventTick);
  const setSelectedSymbol = useMarketStore((s) => s.setSelectedSymbol);
  const setDraftPriceCents = useMarketStore((s) => s.setDraftPriceCents);
  const setBookSnapshot = useMarketStore((s) => s.setBookSnapshot);
  const setInitialTrades = useMarketStore((s) => s.setInitialTrades);

  const instrumentsQuery = useQuery({
    queryKey: ['instruments'],
    queryFn: api.getInstruments,
    refetchInterval: 5000,
  });

  const candlesQuery = useQuery({
    queryKey: ['candles', selectedSymbol, candleInterval],
    queryFn: () => api.getCandles(selectedSymbol, candleInterval, 120),
    refetchInterval: 15000,
  });

  const portfolioQuery = useQuery({
    queryKey: ['portfolio', auth?.user_id, orderEventTick],
    queryFn: api.getPortfolio,
    enabled: Boolean(auth),
    refetchInterval: 4000,
  });

  const ordersQuery = useQuery({
    queryKey: ['orders', auth?.user_id, orderEventTick],
    queryFn: () => api.getOrders(),
    enabled: Boolean(auth),
    refetchInterval: 4000,
  });

  const leaderboardQuery = useQuery({
    queryKey: ['leaderboard', orderEventTick],
    queryFn: api.getLeaderboard,
    enabled: Boolean(auth),
    refetchInterval: 8000,
  });

  useEffect(() => {
    const id = window.setInterval(() => setNowMs(Date.now()), 1000);
    return () => window.clearInterval(id);
  }, []);

  useEffect(() => {
    const eq = portfolioQuery.data?.total_equity;
    if (typeof eq !== 'number') return;
    setEquityHistory((prev) => {
      if (prev.length > 0 && prev[prev.length - 1].equity === eq && prev.length >= 2) {
        return prev;
      }
      return [...prev.slice(-119), { ts: Date.now(), equity: eq }];
    });
  }, [portfolioQuery.data?.total_equity]);

  useEffect(() => {
    if (!auth) return;
    wsClient.start(selectedSymbol);
    return () => wsClient.stop();
  }, [auth?.access_token]);

  useEffect(() => {
    wsClient.switchSymbol(selectedSymbol);
    api
      .getBook(selectedSymbol, 20)
      .then((snap) => setBookSnapshot(snap))
      .catch(() => {});
    api
      .getTrades(selectedSymbol, 40)
      .then((list) => setInitialTrades(selectedSymbol, list))
      .catch(() => {});
  }, [selectedSymbol]);

  const handleAuthSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setAuthError(null);
    try {
      const pair =
        authMode === 'register'
          ? await api.register(email, password)
          : await api.login(email, password);
      setAuth(pair);
    } catch (err) {
      // If registering an existing demo email, fall back to login smoothly
      if (authMode === 'register') {
        try {
          const pair = await api.login(email, password);
          setAuth(pair);
          return;
        } catch {
          // ignore
        }
      }
      setAuthError(err instanceof Error ? err.message : 'Authentication failed');
    }
  };

  const handleLogout = () => {
    wsClient.stop();
    saveAuth(null);
    setAuth(null);
    queryClient.clear();
  };

  const refreshAccountData = () => {
    queryClient.invalidateQueries({ queryKey: ['portfolio'] });
    queryClient.invalidateQueries({ queryKey: ['orders'] });
  };

  const handleCancelOrder = async (orderId: string) => {
    try {
      await api.cancelOrder(orderId);
      refreshAccountData();
    } catch {
      // ignore
    }
  };

  if (!auth) {
    return (
      <div className="auth-screen">
        <div className="auth-card">
          <div className="brand-header">
            <span className="brand-dot" />
            <h1>OrderFlow Exchange</h1>
          </div>
          <p className="auth-subtitle">
            Event-driven paper-trading exchange · Go · Kafka · PostgreSQL · Double-Entry Ledger
          </p>

          <div className="type-toggle">
            <button
              type="button"
              className={`tab-btn ${authMode === 'register' ? 'active' : ''}`}
              onClick={() => setAuthMode('register')}
            >
              Register ($100k Virtual Cash)
            </button>
            <button
              type="button"
              className={`tab-btn ${authMode === 'login' ? 'active' : ''}`}
              onClick={() => setAuthMode('login')}
            >
              Sign In
            </button>
          </div>

          <form onSubmit={handleAuthSubmit} className="ticket-form">
            <label className="field">
              <span>Email</span>
              <input
                type="email"
                required
                value={email}
                onChange={(e) => setEmail(e.target.value)}
              />
            </label>
            <label className="field">
              <span>Password</span>
              <input
                type="password"
                required
                minLength={6}
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
            </label>
            <button type="submit" className="submit-order-btn buy-submit">
              {authMode === 'register' ? 'Create Paper Account & Enter Terminal' : 'Enter Terminal'}
            </button>
            {authError && <div className="ticket-feedback err">{authError}</div>}
          </form>
        </div>
      </div>
    );
  }

  const instruments = instrumentsQuery.data || [DEFAULT_INSTRUMENT];
  const currentInstrument =
    instruments.find((i) => i.symbol === selectedSymbol) || instruments[0] || DEFAULT_INSTRUMENT;
  const currentBook = books[selectedSymbol] || {
    symbol: selectedSymbol,
    seq: 0,
    bids: [],
    asks: [],
  };
  const currentTrades = tradesMap[selectedSymbol] || [];
  const livePrice =
    lastPrices[selectedSymbol] || currentInstrument.last_price || currentInstrument.ref_price;

  const portfolio = portfolioQuery.data;
  const currentPos = portfolio?.positions.find((p) => p.symbol === selectedSymbol);
  const wsHaltUntilMs = haltedUntilMap[selectedSymbol] || 0;
  const restHaltUntilMs = currentInstrument.halted_until
    ? Math.floor(currentInstrument.halted_until / 1_000_000)
    : 0;
  const activeHaltUntilMs = Math.max(wsHaltUntilMs, restHaltUntilMs);
  const isSymbolHalted =
    currentInstrument.status === 'HALTED' || (activeHaltUntilMs > 0 && activeHaltUntilMs > nowMs);

  return (
    <div className="terminal-layout">
      <header className="topbar">
        <div className="topbar-left">
          <div className="brand-inline">
            <span className="brand-dot" />
            <span className="brand-title">ORDERFLOW</span>
          </div>

          <div className="symbol-strip">
            {instruments.map((inst) => {
              const px = lastPrices[inst.symbol] || inst.last_price || inst.ref_price;
              const active = inst.symbol === selectedSymbol;
              return (
                <button
                  key={inst.symbol}
                  type="button"
                  className={`sym-chip ${active ? 'active' : ''}`}
                  onClick={() => setSelectedSymbol(inst.symbol)}
                >
                  <span className="sym-name">{inst.symbol}</span>
                  <span className="sym-px mono">${formatPrice(px)}</span>
                </button>
              );
            })}
          </div>

          {isSymbolHalted && (
            <div
              className="luld-halt-badge mono bold"
              style={{
                background: 'rgba(239, 68, 68, 0.2)',
                border: '1px solid #ef4444',
                color: '#fca5a5',
                padding: '4px 10px',
                borderRadius: '4px',
                fontSize: '11px',
                letterSpacing: '0.04em',
              }}
            >
              ⚠ TRADING HALTED (LULD)
              {activeHaltUntilMs > nowMs
                ? ` · ${Math.ceil((activeHaltUntilMs - nowMs) / 1000)}s`
                : ''}
            </div>
          )}
        </div>

        <div className="topbar-right">
          {portfolio && (
            <div className="kpi-group">
              <div className="kpi">
                <span className="kpi-label">Total Equity</span>
                <span className="kpi-val mono">{formatUSD(portfolio.total_equity)}</span>
              </div>
              <div className="kpi">
                <span className="kpi-label">Avail Cash</span>
                <span className="kpi-val mono">{formatUSD(portfolio.cash_available)}</span>
              </div>
              <div className="kpi">
                <span className="kpi-label">Unrealised P&amp;L</span>
                <span
                  className={`kpi-val mono ${
                    portfolio.total_unrealised_pnl >= 0 ? 'pnl-pos' : 'pnl-neg'
                  }`}
                >
                  {portfolio.total_unrealised_pnl >= 0 ? '+' : ''}
                  {formatUSD(portfolio.total_unrealised_pnl)}
                </span>
              </div>
            </div>
          )}

          <div className={`ws-status ${connected ? 'ws-live' : 'ws-down'}`}>
            <span className="ws-dot" />
            <span>{connected ? 'WS LIVE' : 'RECONNECTING'}</span>
          </div>

          <span className="user-pill mono">{auth.email}</span>
          <button type="button" className="ghost-btn" onClick={handleLogout}>
            Sign Out
          </button>
        </div>
      </header>

      <main className="workspace-grid">
        <PriceChart
          symbol={selectedSymbol}
          interval={candleInterval}
          onChangeInterval={setCandleInterval}
          candles={candlesQuery.data || []}
          liveCandle={
            latestCandle[selectedSymbol]?.interval === candleInterval
              ? latestCandle[selectedSymbol]
              : undefined
          }
        />

        <OrderBook
          symbol={selectedSymbol}
          bids={currentBook.bids}
          asks={currentBook.asks}
          lastPrice={livePrice}
          onSelectPrice={(px) => setDraftPriceCents(px)}
        />

        <TradeTape symbol={selectedSymbol} trades={currentTrades} />

        <OrderTicket
          instrument={{
            ...currentInstrument,
            last_price: livePrice,
            status: isSymbolHalted ? 'HALTED' : currentInstrument.status,
          }}
          draftPriceCents={draftPriceCents}
          cashAvailable={portfolio?.cash_available ?? 0}
          sharesAvailable={currentPos?.qty ?? 0}
          onOrderPlaced={refreshAccountData}
        />

        <PositionsTable
          positions={portfolio?.positions || []}
          orders={ordersQuery.data || []}
          leaderboard={leaderboardQuery.data || []}
          equityHistory={equityHistory}
          currentUserId={auth.user_id}
          onCancelOrder={handleCancelOrder}
          onSelectSymbol={setSelectedSymbol}
        />
      </main>
    </div>
  );
};
