import React, { useEffect, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api, loadAuth, saveAuth } from './api';
import { OrderBook } from './components/OrderBook';
import { OrderTicket } from './components/OrderTicket';
import {
  PositionsTable,
  type BottomTab,
  type EquityPoint,
} from './components/PositionsTable';
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

const OrderFlowExchangeLogo: React.FC<{ size?: number }> = ({ size = 34 }) => (
  <svg
    width={size}
    height={size}
    viewBox="0 0 40 40"
    fill="none"
    xmlns="http://www.w3.org/2000/svg"
    style={{ flexShrink: 0 }}
  >
    <rect width="40" height="40" rx="10" fill="#181A20" />
    {/* Bid side bars (left, emerald) */}
    <rect x="6" y="10" width="12" height="4" rx="2" fill="#00D09C" />
    <rect x="6" y="18" width="9" height="4" rx="2" fill="#00D09C" />
    <rect x="6" y="26" width="14" height="4" rx="2" fill="#00D09C" />
    {/* Ask side bars (right, coral) */}
    <rect x="22" y="10" width="12" height="4" rx="2" fill="#EB5B3C" />
    <rect x="25" y="18" width="9" height="4" rx="2" fill="#EB5B3C" />
    <rect x="20" y="26" width="14" height="4" rx="2" fill="#EB5B3C" />
    {/* Center matching axis */}
    <rect x="19.25" y="7" width="1.5" height="26" rx="0.75" fill="rgba(255,255,255,0.35)" />
  </svg>
);

export const App: React.FC = () => {
  const [auth, setAuth] = useState<TokenPair | null>(() => loadAuth());
  const [authMode, setAuthMode] = useState<'login' | 'register'>('register');
  const [email, setEmail] = useState('trader@orderflow.dev');
  const [password, setPassword] = useState('trader123');
  const [authError, setAuthError] = useState<string | null>(null);

  const [theme, setTheme] = useState<'light' | 'dark'>(() => {
    const saved = localStorage.getItem('orderflow.theme');
    return saved === 'dark' ? 'dark' : 'light';
  });
  const [navSection, setNavSection] = useState<
    'explore' | 'investments' | 'orders' | 'leaderboard'
  >('explore');
  const [bottomTab, setBottomTab] = useState<BottomTab>('positions');
  const [searchQuery, setSearchQuery] = useState('');
  const [searchOpen, setSearchOpen] = useState(false);
  const [watchlisted, setWatchlisted] = useState<Record<string, boolean>>({
    AAPL: true,
    NVDA: true,
  });

  const [candleInterval, setCandleInterval] = useState<CandleInterval>('1m');
  const [equityHistory, setEquityHistory] = useState<EquityPoint[]>([]);
  const [nowMs, setNowMs] = useState<number>(() => Date.now());

  useEffect(() => {
    document.documentElement.setAttribute('data-theme', theme);
    localStorage.setItem('orderflow.theme', theme);
  }, [theme]);

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
    queryClient.invalidateQueries({ queryKey: ['leaderboard'] });
  };

  const handleCancelOrder = async (orderId: string) => {
    try {
      await api.cancelOrder(orderId);
      refreshAccountData();
    } catch {
      // ignore
    }
  };

  const handleNavClick = (
    sec: 'explore' | 'investments' | 'orders' | 'leaderboard',
    targetTab: BottomTab
  ) => {
    setNavSection(sec);
    setBottomTab(targetTab);
    if (sec !== 'explore') {
      const el = document.getElementById('groww-bottom-section');
      el?.scrollIntoView({ behavior: 'smooth', block: 'start' });
    } else {
      window.scrollTo({ top: 0, behavior: 'smooth' });
    }
  };

  // ---- OrderFlow Stock Exchange Login / Onboarding Screen -------------------
  if (!auth) {
    return (
      <div className="groww-auth-backdrop">
        <div className="groww-auth-modal">
          <div className="groww-auth-promo">
            <div className="groww-auth-promo-top">
              <div className="groww-brand-row">
                <OrderFlowExchangeLogo size={40} />
                <span className="groww-promo-brand">OrderFlow Exchange</span>
              </div>
              <h2>Electronic Central Limit Order Book &amp; Matching Engine.</h2>
              <p>
                Direct Market Access (DMA) to a deterministic price-time priority stock
                exchange with real-time L2 depth, LULD circuit halts, and double-entry clearing.
              </p>
            </div>
            <div className="groww-auth-features">
              <div className="groww-feature-pill">✓ $100,000 Clearing Member Margin</div>
              <div className="groww-feature-pill">✓ Limit, Market, Stop &amp; Stop-Limit Book</div>
              <div className="groww-feature-pill">✓ LULD ±10% Exchange Circuit Breakers</div>
            </div>
          </div>

          <div className="groww-auth-form-pane">
            <div className="groww-auth-header">
              <h3>OrderFlow Stock Exchange (OFX)</h3>
              <p className="muted">Authenticate your trading member session to access the CLOB</p>
            </div>

            <div className="groww-pad-side-tabs" style={{ marginBottom: '18px' }}>
              <button
                type="button"
                className={`groww-side-tab buy-tab ${authMode === 'register' ? 'active' : ''}`}
                onClick={() => setAuthMode('register')}
              >
                New Member ($100k Margin)
              </button>
              <button
                type="button"
                className={`groww-side-tab buy-tab ${authMode === 'login' ? 'active' : ''}`}
                onClick={() => setAuthMode('login')}
              >
                Member Sign In
              </button>
            </div>

            <form onSubmit={handleAuthSubmit} className="groww-login-form">
              <label className="groww-login-field">
                <span>Member Email ID</span>
                <input
                  type="email"
                  required
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  placeholder="Enter member email"
                />
              </label>
              <label className="groww-login-field">
                <span>Access Passphrase</span>
                <input
                  type="password"
                  required
                  minLength={6}
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  placeholder="Enter passphrase"
                />
              </label>

              <button type="submit" className="groww-submit-btn groww-buy-btn">
                {authMode === 'register'
                  ? 'Register Member & Enter Exchange'
                  : 'Connect to Exchange Matching Engine'}
              </button>
              {authError && <div className="groww-feedback err">{authError}</div>}
            </form>
          </div>
        </div>
      </div>
    );
  }

  const instruments = instrumentsQuery.data || [DEFAULT_INSTRUMENT];
  const currentInstrument =
    instruments.find((i) => i.symbol === selectedSymbol) ||
    instruments[0] ||
    DEFAULT_INSTRUMENT;
  const currentBook = books[selectedSymbol] || {
    symbol: selectedSymbol,
    seq: 0,
    bids: [],
    asks: [],
  };
  const currentTrades = tradesMap[selectedSymbol] || [];
  const livePrice =
    lastPrices[selectedSymbol] ||
    currentInstrument.last_price ||
    currentInstrument.ref_price;
  const refPrice = currentInstrument.ref_price || livePrice;
  const priceDiff = livePrice - refPrice;
  const priceDiffPct = refPrice > 0 ? (priceDiff / refPrice) * 100 : 0;
  const isUp = priceDiff >= 0;

  const portfolio = portfolioQuery.data;
  const currentPos = portfolio?.positions.find((p) => p.symbol === selectedSymbol);
  const wsHaltUntilMs = haltedUntilMap[selectedSymbol] || 0;
  const restHaltUntilMs = currentInstrument.halted_until
    ? Math.floor(currentInstrument.halted_until / 1_000_000)
    : 0;
  const activeHaltUntilMs = Math.max(wsHaltUntilMs, restHaltUntilMs);
  const isSymbolHalted =
    currentInstrument.status === 'HALTED' ||
    (activeHaltUntilMs > 0 && activeHaltUntilMs > nowMs);

  // Compute day stats from candles for Groww's Performance section
  const candleList = candlesQuery.data || [];
  const dayHigh =
    candleList.length > 0
      ? Math.max(livePrice, ...candleList.map((c) => c.high))
      : Math.round(refPrice * 1.02);
  const dayLow =
    candleList.length > 0
      ? Math.min(livePrice, ...candleList.map((c) => c.low))
      : Math.round(refPrice * 0.98);
  const dayOpen = candleList.length > 0 ? candleList[0].open : refPrice;
  const dayVolume = candleList.reduce((acc, c) => acc + c.volume, 0);
  const rangeSpan = Math.max(1, dayHigh - dayLow);
  const dayRangePct = Math.min(100, Math.max(0, ((livePrice - dayLow) / rangeSpan) * 100));

  const luldLow = Math.round(refPrice * 0.9);
  const luldHigh = Math.round(refPrice * 1.1);
  const luldSpan = Math.max(1, luldHigh - luldLow);
  const luldRangePct = Math.min(100, Math.max(0, ((livePrice - luldLow) / luldSpan) * 100));

  const filteredInstruments = instruments.filter(
    (inst) =>
      inst.symbol.toLowerCase().includes(searchQuery.toLowerCase()) ||
      inst.name.toLowerCase().includes(searchQuery.toLowerCase())
  );

  return (
    <div className="groww-app">
      {/* OrderFlow Exchange Top Navigation Bar */}
      <header className="groww-navbar">
        <div className="groww-navbar-inner">
          <div className="groww-nav-left">
            <div
              className="groww-brand"
              onClick={() => handleNavClick('explore', 'positions')}
            >
              <OrderFlowExchangeLogo size={34} />
              <div className="groww-brand-text">
                <span className="groww-brand-name">OrderFlow</span>
                <span className="groww-brand-tag">EXCHANGE · CLOB</span>
              </div>
            </div>

            <nav className="groww-nav-links">
              <button
                type="button"
                className={`groww-nav-link ${navSection === 'explore' ? 'active' : ''}`}
                onClick={() => handleNavClick('explore', 'positions')}
              >
                Market Watch
              </button>
              <button
                type="button"
                className={`groww-nav-link ${navSection === 'investments' ? 'active' : ''}`}
                onClick={() => handleNavClick('investments', 'positions')}
              >
                Net Positions
              </button>
              <button
                type="button"
                className={`groww-nav-link ${navSection === 'orders' ? 'active' : ''}`}
                onClick={() => handleNavClick('orders', 'open_orders')}
              >
                Order Blotter
              </button>
              <button
                type="button"
                className={`groww-nav-link ${navSection === 'leaderboard' ? 'active' : ''}`}
                onClick={() => handleNavClick('leaderboard', 'leaderboard')}
              >
                Member Ranks
              </button>
            </nav>
          </div>

          {/* Exchange Instrument Search Bar */}
          <div className="groww-search-container">
            <span className="groww-search-icon">🔍</span>
            <input
              type="text"
              className="groww-search-input"
              placeholder="Search listed instruments on OFX (AAPL, NVDA, MSFT…)"
              value={searchQuery}
              onFocus={() => setSearchOpen(true)}
              onBlur={() => window.setTimeout(() => setSearchOpen(false), 180)}
              onChange={(e) => {
                setSearchQuery(e.target.value);
                setSearchOpen(true);
              }}
            />
            {searchOpen && (
              <div className="groww-search-dropdown">
                {filteredInstruments.map((inst) => {
                  const px = lastPrices[inst.symbol] || inst.last_price || inst.ref_price;
                  return (
                    <div
                      key={inst.symbol}
                      className="groww-search-item"
                      onMouseDown={() => {
                        setSelectedSymbol(inst.symbol);
                        setSearchQuery('');
                        setSearchOpen(false);
                      }}
                    >
                      <div className="groww-company-cell">
                        <span className="groww-mini-logo">{inst.symbol.slice(0, 2)}</span>
                        <div>
                          <div className="bold">{inst.name}</div>
                          <div className="muted" style={{ fontSize: '11px' }}>
                            OFX · EQ · {inst.symbol}
                          </div>
                        </div>
                      </div>
                      <span className="mono bold">${formatPrice(px)}</span>
                    </div>
                  );
                })}
              </div>
            )}
          </div>

          <div className="groww-nav-right">
            {portfolio && (
              <div className="groww-wallet-pill" title="Available Clearing Margin">
                <span className="muted">Margin</span>
                <span className="mono bold">{formatUSD(portfolio.cash_available)}</span>
              </div>
            )}

            <div className={`groww-ws-badge ${connected ? 'live' : 'down'}`}>
              <span className="groww-ws-dot" />
              <span>{connected ? 'LIVE' : 'SYNC'}</span>
            </div>

            <button
              type="button"
              className="groww-icon-btn"
              title="Toggle Light / Dark Theme"
              onClick={() => setTheme((t) => (t === 'light' ? 'dark' : 'light'))}
            >
              {theme === 'light' ? '🌙' : '☀️'}
            </button>

            <div className="groww-user-chip">
              <span className="groww-avatar">
                {auth.email.slice(0, 1).toUpperCase()}
              </span>
              <span className="groww-user-email">{auth.email.split('@')[0]}</span>
              <button
                type="button"
                className="groww-logout-btn"
                onClick={handleLogout}
              >
                Logout
              </button>
            </div>
          </div>
        </div>
      </header>

      {/* Groww Indices & Watchlist Strip */}
      <div className="groww-ticker-bar">
        <div className="groww-ticker-inner">
          {instruments.map((inst) => {
            const px = lastPrices[inst.symbol] || inst.last_price || inst.ref_price;
            const ref = inst.ref_price || px;
            const diff = px - ref;
            const pct = ref > 0 ? (diff / ref) * 100 : 0;
            const active = inst.symbol === selectedSymbol;
            return (
              <button
                key={inst.symbol}
                type="button"
                className={`groww-stock-chip ${active ? 'active' : ''}`}
                onClick={() => setSelectedSymbol(inst.symbol)}
              >
                <div className="groww-chip-top">
                  <span className="groww-chip-sym">{inst.symbol}</span>
                  <span className="groww-chip-px mono">${formatPrice(px)}</span>
                </div>
                <div className={`groww-chip-chg mono ${diff >= 0 ? 'pnl-pos' : 'pnl-neg'}`}>
                  {diff >= 0 ? '+' : ''}
                  {formatPrice(diff)} ({diff >= 0 ? '+' : ''}
                  {pct.toFixed(2)}%)
                </div>
              </button>
            );
          })}
        </div>
      </div>

      {/* Main Groww Two-Column Container */}
      <main className="groww-main-container">
        <div className="groww-left-column">
          {/* Stock Header + Interactive Chart Card */}
          <div className="groww-card groww-stock-hero-card">
            <div className="groww-stock-header">
              <div className="groww-stock-identity">
                <div className="groww-stock-logo-box">
                  {currentInstrument.symbol.slice(0, 2)}
                </div>
                <div>
                  <div className="groww-stock-title-row">
                    <h1 className="groww-stock-name">{currentInstrument.name}</h1>
                    <span className="groww-symbol-pill">{currentInstrument.symbol}</span>
                    {isSymbolHalted && (
                      <span className="groww-luld-banner mono">
                        ⚠ TRADING HALTED (LULD)
                        {activeHaltUntilMs > nowMs
                          ? ` · ${Math.ceil((activeHaltUntilMs - nowMs) / 1000)}s`
                          : ''}
                      </span>
                    )}
                  </div>
                  <div className="groww-price-hero-row">
                    <span className="groww-big-price mono">
                      ${formatPrice(livePrice)}
                    </span>
                    <span
                      className={`groww-price-change mono ${
                        isUp ? 'pnl-pos' : 'pnl-neg'
                      }`}
                    >
                      {isUp ? '+' : ''}
                      {formatPrice(priceDiff)} ({isUp ? '+' : ''}
                      {priceDiffPct.toFixed(2)}%) <span className="muted">1D</span>
                    </span>
                  </div>
                </div>
              </div>

              <div className="groww-stock-actions">
                <button
                  type="button"
                  className={`groww-action-pill ${
                    watchlisted[selectedSymbol] ? 'saved' : ''
                  }`}
                  onClick={() =>
                    setWatchlisted((w) => ({
                      ...w,
                      [selectedSymbol]: !w[selectedSymbol],
                    }))
                  }
                >
                  {watchlisted[selectedSymbol] ? '★ Watchlisted' : '☆ Watchlist'}
                </button>
              </div>
            </div>

            <PriceChart
              symbol={selectedSymbol}
              interval={candleInterval}
              onChangeInterval={setCandleInterval}
              candles={candleList}
              liveCandle={
                latestCandle[selectedSymbol]?.interval === candleInterval
                  ? latestCandle[selectedSymbol]
                  : undefined
              }
              theme={theme}
            />
          </div>

          {/* Groww Performance & Exchange Rules Card */}
          <div className="groww-card groww-performance-card">
            <h3 className="groww-section-title">Performance &amp; Circuit Bands</h3>

            <div className="groww-range-bars">
              <div className="groww-range-item">
                <div className="groww-range-labels">
                  <div>
                    <span className="muted">Today&apos;s Low</span>
                    <div className="mono bold">${formatPrice(dayLow)}</div>
                  </div>
                  <div className="text-right">
                    <span className="muted">Today&apos;s High</span>
                    <div className="mono bold">${formatPrice(dayHigh)}</div>
                  </div>
                </div>
                <div className="groww-range-track">
                  <div
                    className="groww-range-fill"
                    style={{ width: `${dayRangePct}%` }}
                  />
                  <div
                    className="groww-range-thumb"
                    style={{ left: `${dayRangePct}%` }}
                  />
                </div>
              </div>

              <div className="groww-range-item">
                <div className="groww-range-labels">
                  <div>
                    <span className="muted">LULD Lower Band (-10%)</span>
                    <div className="mono bold">${formatPrice(luldLow)}</div>
                  </div>
                  <div className="text-right">
                    <span className="muted">LULD Upper Band (+10%)</span>
                    <div className="mono bold">${formatPrice(luldHigh)}</div>
                  </div>
                </div>
                <div className="groww-range-track">
                  <div
                    className="groww-range-fill"
                    style={{ width: `${luldRangePct}%` }}
                  />
                  <div
                    className="groww-range-thumb"
                    style={{ left: `${luldRangePct}%` }}
                  />
                </div>
              </div>
            </div>

            <div className="groww-fundamentals-grid">
              <div className="groww-stat-cell">
                <span className="muted">Open</span>
                <span className="mono bold">${formatPrice(dayOpen)}</span>
              </div>
              <div className="groww-stat-cell">
                <span className="muted">Prev. Close (Ref)</span>
                <span className="mono bold">${formatPrice(refPrice)}</span>
              </div>
              <div className="groww-stat-cell">
                <span className="muted">Volume</span>
                <span className="mono bold">{dayVolume.toLocaleString()}</span>
              </div>
              <div className="groww-stat-cell">
                <span className="muted">Tick Size</span>
                <span className="mono bold">${formatPrice(currentInstrument.tick_size)}</span>
              </div>
              <div className="groww-stat-cell">
                <span className="muted">Lot Size</span>
                <span className="mono bold">{currentInstrument.lot_size} sh</span>
              </div>
              <div className="groww-stat-cell">
                <span className="muted">Max Order Qty</span>
                <span className="mono bold">
                  {currentInstrument.max_order_qty.toLocaleString()}
                </span>
              </div>
            </div>
          </div>

          {/* Market Depth & Live Trades Sub-Grid */}
          <div className="groww-depth-tape-grid">
            <OrderBook
              symbol={selectedSymbol}
              bids={currentBook.bids}
              asks={currentBook.asks}
              lastPrice={livePrice}
              onSelectPrice={(px) => setDraftPriceCents(px)}
            />

            <TradeTape symbol={selectedSymbol} trades={currentTrades} />
          </div>

          {/* Holdings, Orders, Leaderboard & Equity Curve */}
          <div id="groww-bottom-section">
            <PositionsTable
              positions={portfolio?.positions || []}
              orders={ordersQuery.data || []}
              leaderboard={leaderboardQuery.data || []}
              equityHistory={equityHistory}
              currentUserId={auth.user_id}
              activeTab={bottomTab}
              onChangeTab={setBottomTab}
              onCancelOrder={handleCancelOrder}
              onOrderAmended={refreshAccountData}
              onSelectSymbol={setSelectedSymbol}
            />
          </div>
        </div>

        {/* Sticky Right Column: Clearing Account Summary + DMA Order Entry Pad */}
        <aside className="groww-right-column">
          {portfolio && (
            <div className="groww-card groww-portfolio-summary-card">
              <div className="groww-summary-card-head">
                <span className="groww-summary-label">Member Clearing Account</span>
                <button
                  type="button"
                  className="groww-link-btn"
                  onClick={() => handleNavClick('investments', 'positions')}
                >
                  Net Positions →
                </button>
              </div>
              <div className="groww-summary-metrics">
                <div>
                  <div className="muted" style={{ fontSize: '11px' }}>
                    Total Member Equity
                  </div>
                  <div className="groww-summary-big mono">
                    {formatUSD(portfolio.total_equity)}
                  </div>
                </div>
                <div className="text-right">
                  <div className="muted" style={{ fontSize: '11px' }}>
                    Unrealised MTM P&amp;L
                  </div>
                  <div
                    className={`groww-summary-big mono ${
                      portfolio.total_unrealised_pnl >= 0 ? 'pnl-pos' : 'pnl-neg'
                    }`}
                  >
                    {portfolio.total_unrealised_pnl >= 0 ? '+' : ''}
                    {formatUSD(portfolio.total_unrealised_pnl)}
                  </div>
                </div>
              </div>
            </div>
          )}

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
        </aside>
      </main>
    </div>
  );
};
