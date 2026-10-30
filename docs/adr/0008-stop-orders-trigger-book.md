# ADR-008: Stop-Loss & Stop-Limit Orders (Trigger Book)

- **Status:** Accepted
- **Date:** 2026-10-30

## Context

Traders need conditional stop-loss (`STOP`) and stop-limit (`STOP_LIMIT`) orders that remain dormant off the visible L2 order book until the market's last trade price (`lastTradePrice`) reaches or crosses a user-specified trigger price (`stop_price`).

Key design requirements:
1. **Trigger Ordering:**
   - **Buy Stop / Buy Stop-Limit:** Triggers when `lastTradePrice >= stop_price` (used for breakout entries or covering short exposure). Lowest `stop_price` triggers first as price rises.
   - **Sell Stop / Sell Stop-Limit:** Triggers when `lastTradePrice <= stop_price` (used for downside stop-loss protection). Highest `stop_price` triggers first as price falls.
2. **Pre-crossed Protection:** If a stop order arrives when `lastTradePrice` has already crossed `stop_price`, rejecting it with `STOP_WOULD_TRIGGER` (`ReasonStopHit`) prevents accidental immediate market execution from a typo.
3. **Cascading Triggers:** A triggered stop order is injected into the active matching book (`STOP` as `MARKET`, `STOP_LIMIT` as `LIMIT`). Its trades may move `lastTradePrice` further, electing additional stop orders within the same deterministic command cycle.
4. **Deterministic Crash Recovery:** Both `lastTradePrice` and all resting stop levels must be captured in partition snapshots (`internal/engine/snapshot.go`) so recovery + Kafka tail replay produces identical event streams.

## Decision

1. **Dedicated Trigger Book (`internal/engine/book.go`):**
   - Maintain `buyStops` (B-Tree sorted ascending by `stop_price`) and `sellStops` (B-Tree sorted descending by `stop_price`), with intrusive doubly-linked FIFO queues (`stopLevel`) at each trigger price and a `map[uuid.UUID]*stopOrder` for $O(1)$ cancellation.
   - Untriggered stops emit `EvOrder` with `Status: ACCEPTED` and `Resting: false` so downstream L2 depth projections (`internal/gateway`) do not display them in public book depth until elected.

2. **Election & Injection (`internal/engine/engine.go`):**
   - Every trade updates `b.lastTradePrice = px`.
   - At the end of any command (`PLACE` or `REPLACE`) that produces trades, `triggerStops(b)` loops until `electTriggeredStops()` returns empty, injecting each elected stop into `match()` while preserving its original `STOP` / `STOP_LIMIT` type in emitted execution reports.

3. **Upfront Reservation (`internal/order/service.go`):**
   - `BUY STOP_LIMIT` reserves `price * qty` cents.
   - `BUY STOP` reserves `ComputeProtectionPrice(stop_price, tick_size) * qty` cents (5% protection collar above `stop_price`).
   - `SELL STOP` and `SELL STOP_LIMIT` reserve `qty` shares.

## Consequences

- **Pros:**
  - $O(\log S)$ trigger election and $O(1)$ stop cancellation by `order_id`.
  - Full deterministic snapshot/restore invariants (`checkStopInvariants`).
  - Seamless settlement via existing double-entry ledger and price-improvement refund logic.
- **Cons:**
  - Cascading stop elections execute synchronously within the triggering command's partition goroutine.
