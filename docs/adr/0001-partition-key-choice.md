# ADR-001: Partition Key Choice (`symbol` for Commands and Trades)

- **Status:** Accepted
- **Date:** 2026-10-28

## Context

A limit order book is inherently sequential per trading instrument: whether order $B$ matches resting order $A$ depends on the exact arrival order of commands for that instrument. Kafka guarantees total ordering **only within a single partition**.

## Decision

- `orders.commands`, `trades.executed`, `market.ticks`, and `market.candles.1m` use **`symbol`** as the Kafka record key.
- `orders.events` uses `order_id` as the key, while carrying a per-symbol monotonic `seq` inside the payload so downstream order-book projections can detect gaps.
- Inside `matching-engine`, each assigned partition of `orders.commands` is owned by **one dedicated goroutine** with its own `engine.Engine` instance. No mutexes are required on the hot path.

## Consequences

- **Pros:** Strict per-symbol FIFO ordering; zero lock contention in the matching engine; horizontal scale across symbols up to the partition count.
- **Cons / Hot-symbol trade-off:** One symbol cannot be split across partitions without breaking price-time priority. Because the single-threaded Go engine processes >500k commands/sec per core (see `docs/benchmarks.md`), a single partition easily saturates network/broker limits before CPU becomes the bottleneck.

