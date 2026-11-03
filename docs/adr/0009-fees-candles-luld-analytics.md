# ADR 0009: Maker-Taker Fees, Multi-Timeframe Candlesticks, LULD Circuit Breakers & Portfolio Analytics

- **Status:** Accepted
- **Date:** 2026-10-30

## Context & Decisions

### 1. Maker-Taker Fee Tier & Exchange Fee Revenue Account (`feat/maker-taker-fees`)
- **System Account:** Migration `0004_fees.up.sql` seeds the deterministic `EXCHANGE_FEES` user (`00000000-0000-0000-0000-00000000fee5`) and its USD account (`system:exchange_fees`), and adds `maker_fee` and `taker_fee` (`BIGINT` minor units) to `trades`.
- **Double-Entry Settlement:** `ledger.Service` (`WithFees(makerBps, takerBps)`) computes `takerFee` (default `+5 bps`) and `makerRebate` (default `-1 bps`) on each trade's notional (`price * qty`). In addition to the principal USD and equity share double-entry pairs, `ApplyTrade` writes balanced `DEBIT`/`CREDIT` entries between the taker/maker accounts and `system:exchange_fees`, preserving the zero-sum global invariant (`SUM(credit - debit) == 0` per asset) verified by `ledger.Reconcile`.

### 2. Multi-Timeframe Candlesticks (`1m`, `5m`, `15m`, `1h`) + Volume Histogram (`feat/multi-timeframe-candles`)
- **Market Data Aggregation:** `marketdata.Service` maintains in-memory and PostgreSQL-persisted OHLCV buckets across four intervals (`1m = 60s`, `5m = 300s`, `15m = 900s`, `1h = 3600s`).
- **Gateway & Chart UI:** `GET /api/v1/candles/{symbol}?interval=1m|5m|15m|1h` serves historical bars for the selected timeframe, and `PriceChart.tsx` renders interactive `1m | 5m | 15m | 1h` selector buttons alongside a `lightweight-charts` volume histogram overlay.

### 3. Circuit Breakers & Limit-Up / Limit-Down (LULD) Trading Halts (`feat/circuit-breakers`)
- **Deterministic Engine LULD Band:** `engine.Engine` (`WithCircuitBreaker(bandBps, windowNs, cooldownNs)`) tracks a rolling 5-minute reference price (`refPrice`, `refPriceTs`) per `OrderBook`. If a trade executes outside $\pm 10\%$ (`1000 bps`) of `refPrice`, the book transitions to `HALTED` until `cmd.Timestamp + 30s` and emits `EvSymbolHalted` (`SYMBOL_HALTED`).
- **Order Rejection & Recovery:** Incoming `PLACE` and `REPLACE` commands during an active halt are rejected with `ReasonHalted` (`SYMBOL_HALTED`). LULD state (`RefPx`, `RefTS`, `HaltUntil`) is included in `engine.Snapshot` for deterministic crash recovery.
- **Live UI Banner:** `ws.Hub` broadcasts `symbol.halted` events to connected terminals, and `App.tsx` displays a live `"TRADING HALTED (LULD)"` countdown badge while disabling order submission on the halted symbol.

### 4. Portfolio Equity Curve, Leaderboard & Trade History CSV Export (`feat/portfolio-analytics`)
- **REST Endpoints:** `api-gateway` exposes `GET /api/v1/leaderboard` (top traders ranked by `available + reserved + sum(position * mark_price)`) and `GET /api/v1/account/fills.csv` (streaming RFC 4180 CSV export of the authenticated trader's fills with role, notional, and fee columns).
- **Terminal Analytics UI:** `PositionsTable.tsx` adds a **Leaderboard** tab, a live **Equity Curve** SVG chart, and a **Download Fill History (CSV)** action.

