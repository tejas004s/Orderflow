import type {
  BookState,
  Candle,
  Instrument,
  Order,
  Portfolio,
  TokenPair,
  Trade,
} from './types';

const STORAGE_KEY = 'orderflow.auth';

export function loadAuth(): TokenPair | null {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    return raw ? (JSON.parse(raw) as TokenPair) : null;
  } catch {
    return null;
  }
}

export function saveAuth(pair: TokenPair | null): void {
  if (!pair) {
    localStorage.removeItem(STORAGE_KEY);
  } else {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(pair));
  }
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const auth = loadAuth();
  const headers = new Headers(init.headers || {});
  if (!headers.has('Content-Type') && init.body) {
    headers.set('Content-Type', 'application/json');
  }
  if (auth?.access_token) {
    headers.set('Authorization', `Bearer ${auth.access_token}`);
  }

  let res = await fetch(path, { ...init, headers });
  if (res.status === 401 && auth?.refresh_token && !path.includes('/auth/')) {
    const refreshed = await tryRefresh(auth.refresh_token);
    if (refreshed) {
      headers.set('Authorization', `Bearer ${refreshed.access_token}`);
      res = await fetch(path, { ...init, headers });
    }
  }

  if (!res.ok) {
    let detail = `HTTP ${res.status}`;
    try {
      const prob = await res.json();
      detail = prob.detail || prob.title || detail;
    } catch {
      // ignore
    }
    throw new Error(detail);
  }
  return (await res.json()) as T;
}

async function tryRefresh(refreshToken: string): Promise<TokenPair | null> {
  try {
    const res = await fetch('/api/v1/auth/refresh', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ refresh_token: refreshToken }),
    });
    if (!res.ok) {
      saveAuth(null);
      return null;
    }
    const pair = (await res.json()) as TokenPair;
    saveAuth(pair);
    return pair;
  } catch {
    return null;
  }
}

export const api = {
  register: async (email: string, password: string): Promise<TokenPair> => {
    const pair = await request<TokenPair>('/api/v1/auth/register', {
      method: 'POST',
      body: JSON.stringify({ email, password }),
    });
    saveAuth(pair);
    return pair;
  },

  login: async (email: string, password: string): Promise<TokenPair> => {
    const pair = await request<TokenPair>('/api/v1/auth/login', {
      method: 'POST',
      body: JSON.stringify({ email, password }),
    });
    saveAuth(pair);
    return pair;
  },

  getInstruments: async (): Promise<Instrument[]> => {
    const res = await request<{ instruments: Instrument[] }>('/api/v1/instruments');
    return res.instruments;
  },

  getBook: async (symbol: string, depth = 20): Promise<BookState> => {
    return request<BookState>(`/api/v1/book/${symbol}?depth=${depth}`);
  },

  getTrades: async (symbol: string, limit = 50): Promise<Trade[]> => {
    const res = await request<{ trades: Trade[] }>(
      `/api/v1/trades?symbol=${encodeURIComponent(symbol)}&limit=${limit}`
    );
    return res.trades;
  },

  getCandles: async (symbol: string, interval = '1m', limit = 120): Promise<Candle[]> => {
    const res = await request<{ candles: Candle[] }>(
      `/api/v1/candles/${symbol}?interval=${interval}&limit=${limit}`
    );
    return res.candles;
  },

  getPortfolio: async (): Promise<Portfolio> => {
    return request<Portfolio>('/api/v1/portfolio');
  },

  seedPortfolio: async (params: {
    cash_cents?: number;
    symbol?: string;
    shares?: number;
  }): Promise<void> => {
    await request('/api/v1/portfolio/seed', {
      method: 'POST',
      body: JSON.stringify(params),
    });
  },

  getOrders: async (symbol = '', status = ''): Promise<Order[]> => {
    const q = new URLSearchParams();
    if (symbol) q.set('symbol', symbol);
    if (status) q.set('status', status);
    const res = await request<{ orders: Order[] }>(`/api/v1/orders?${q.toString()}`);
    return res.orders;
  },

  placeOrder: async (params: {
    symbol: string;
    side: 'BUY' | 'SELL';
    type: 'LIMIT' | 'MARKET';
    price?: number;
    qty: number;
  }): Promise<Order> => {
    const idemKey =
      typeof crypto !== 'undefined' && 'randomUUID' in crypto
        ? crypto.randomUUID()
        : `${Date.now()}-${Math.random()}`;
    return request<Order>('/api/v1/orders', {
      method: 'POST',
      headers: { 'Idempotency-Key': idemKey },
      body: JSON.stringify(params),
    });
  },

  cancelOrder: async (orderId: string): Promise<Order> => {
    return request<Order>(`/api/v1/orders/${orderId}`, {
      method: 'DELETE',
    });
  },
};
