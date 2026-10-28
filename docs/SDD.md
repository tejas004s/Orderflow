# Software Design Document: OrderFlow, an Event-Driven Paper-Trading Exchange

Version 1.0 | Stack: Go, Kafka, PostgreSQL, React (TypeScript)

---

## 1. Purpose and Scope

OrderFlow is a simulated stock exchange. Users place buy/sell orders on virtual money. A matching engine pairs orders, a ledger settles the trades, and a React dashboard shows the live order book, prices, positions and P&L.

**Goal:** demonstrate production-grade backend engineering: event-driven architecture, exactly-once *effects*, crash recovery, and measurable performance.

**In scope:** limit and market orders, cancel, order book, trades, double-entry ledger, positions, simulated market data, live UI, observability, load testing.

**Out of scope:** real money, real broker connectivity, KYC, margin/short selling, options.

**Non-goal disclaimer:** this is a simulation for learning and portfolio use.

---

## 2. Why Each Technology (First Principles)

| Tech | Problem it solves here | Why not the alternative |
|---|---|---|
| **Kafka** | An ordered, durable, replayable log of everything that happens. A matching engine is a deterministic function over an ordered stream of commands. | A plain queue deletes messages after consumption, so you can't replay to rebuild state. |
| **PostgreSQL** | Source of truth for money: ACID transactions, constraints, relational queries. | Kafka cannot enforce "balance never goes negative". Postgres can, with constraints and transactions. |
| **Go** | Cheap concurrency (goroutines/channels), low latency, simple deployment, a natural fit for services and consumers. | |
| **React** | Live-updating UI over WebSockets with high-frequency state changes. | |

**Core principle:** Kafka carries *events about what happened*. Postgres holds *the current state of money*. Never let the two disagree. Section 7 covers how.

---

## 3. Architecture Overview

```
                    +--------------------+
   React SPA  <---> |   api-gateway (Go) |  REST + WebSocket
                    +----+----------+----+
                         |          ^
            (HTTP)       v          | (fan-out from Kafka)
                 +---------------+  |
                 | order-service |  |
                 | validate,     |  |
                 | reserve funds |  |
                 +---+-------+---+  |
          Postgres <--+       | outbox relay
                              v
        +---------------- KAFKA ----------------------------+
        | orders.commands  orders.events  trades.executed   |
        | market.ticks     market.candles.1m   *.dlq        |
        +---+--------------------+---------------+----------+
            |                    |               |
            v                    v               v
   +----------------+   +----------------+  +-----------------+
   | matching-engine|   | ledger-service |  | marketdata-svc  |
   | in-memory books|   | settle trades  |  | simulator +     |
   | per symbol     |   | update balances|  | candle aggreg.  |
   +----------------+   +--------+-------+  +-----------------+
                                 v
                            PostgreSQL
```

### 3.1 Services

| Service | Responsibility | Consumes | Produces |
|---|---|---|---|
| `api-gateway` | Auth (JWT), REST endpoints, WebSocket hub, per-user rate limiting | `orders.events`, `trades.executed`, `market.ticks`, `market.candles.1m` | none (calls order-service over HTTP/gRPC) |
| `order-service` | Validate order, check and reserve funds/shares, persist order as `NEW`, emit command via outbox | none | `orders.commands` (via outbox) |
| `matching-engine` | Maintain order book per symbol, match by price-time priority, emit results | `orders.commands` | `orders.events`, `trades.executed` |
| `ledger-service` | Settle trades atomically: move cash and shares, write double-entry rows, update order status and positions | `trades.executed`, `orders.events` | `ledger.events` (optional) |
| `marketdata-service` | Simulate price ticks per symbol, aggregate OHLCV candles | none (generator), `trades.executed` | `market.ticks`, `market.candles.1m` |

Build these as **one Go module monorepo**, one binary per service, shared `internal/` packages for events, config, logging and DB access.

---

## 4. Kafka Design

### 4.1 Topics

| Topic | Key | Partitions | Retention | Notes |
|---|---|---|---|---|
| `orders.commands` | `symbol` | 6 | 7d | Inputs to matching engine. **Per-symbol ordering is the core guarantee.** |
| `orders.events` | `order_id` | 6 | 7d | Accepted, partially filled, filled, cancelled, rejected |
| `trades.executed` | `symbol` | 6 | 30d | Immutable record of every trade |
| `market.ticks` | `symbol` | 3 | 1h | High-volume, disposable |
| `market.candles.1m` | `symbol` | 3 | compact + 7d | Latest candle per (symbol, minute) |
| `*.dlq` | original key | 1 | 14d | Poison messages with error headers |

### 4.2 Ordering rule

All commands for one symbol go to the same partition, because Kafka only guarantees order within a partition. **One matching-engine goroutine owns each partition.** No locks are needed because there is one writer per order book.

### 4.3 Event envelope (all topics)

Fields: `event_id` (UUID), `event_type`, `schema_version`, `occurred_at`, `producer`, `correlation_id`, `payload`. Use Protobuf or JSON Schema, and pick one. Protobuf is the stronger resume signal. Add a `schema_version` from day one.

### 4.4 Delivery semantics

- Producers: `acks=all`, idempotent producer enabled.
- Consumers: manual offset commit **after** the side effect succeeds (at-least-once).
- Duplicates are made harmless by idempotent handlers (Section 7.2). This is *exactly-once effects* without relying on Kafka transactions across systems.

---

## 5. Matching Engine Design

### 5.1 Order types

`LIMIT` (price, qty) and `MARKET` (qty). Time-in-force: `GTC` for limit; market orders fill immediately or reject unfilled remainder. Cancel is a command.

### 5.2 Matching rule

**Price-time priority:** best price first; at equal price, earliest arrival first. Trade executes at the **resting order's price**.

### 5.3 Data structure

Per symbol, two sides (bids, asks). Each side is a sorted collection of **price levels**; each level is a FIFO queue of orders. Also keep a map `order_id -> order` for O(1) cancel.
Candidate structures for sorted levels: a heap with lazy deletion, or a balanced tree/skip list. Pick one and benchmark; document why in an ADR.

### 5.3.1 Determinism

Same ordered input stream must always produce the same output. No wall-clock reads or random numbers inside the matching logic; timestamps come from the command. This enables replay-based testing.

### 5.4 Crash recovery

1. Periodically write a **snapshot** of each book plus the last processed Kafka offset (to Postgres table `engine_snapshots` or local file).
2. On restart: load snapshot, seek consumer to saved offset, replay.
3. Output events carry a deterministic `event_id` (derived from input command id + sequence), so replays produce duplicates that downstream idempotency discards.

### 5.5 Invariants (to test)

- Book is never crossed (best bid < best ask) after processing a command.
- Total filled qty on buy side equals sell side for every trade.
- No order fills more than its quantity.
- Replay of the same input yields byte-identical output.

---

## 6. PostgreSQL Design

### 6.1 Principles

- Money stored as **integer minor units** (`BIGINT`) or `NUMERIC(20,8)`. Never float.
- Constraints enforce truth: `CHECK (available >= 0)`, foreign keys, unique keys for idempotency.
- Migrations with `golang-migrate`; queries with `sqlc` (type-safe) and `pgx`.

### 6.2 Core tables

**users**: id, email, password_hash, created_at

**accounts**: id, user_id, currency, `available`, `reserved` (both `CHECK >= 0`)

**instruments**: symbol (PK), name, tick_size, lot_size, status

**orders**: id, user_id, symbol, side, type, price, qty, filled_qty, status, created_at, updated_at
Status: `NEW, ACCEPTED, PARTIALLY_FILLED, FILLED, CANCELLED, REJECTED`

**trades**: id, symbol, price, qty, buy_order_id, sell_order_id, executed_at, `UNIQUE(match_id)`

**positions**: user_id, symbol, qty, avg_cost, `PRIMARY KEY(user_id, symbol)`, `CHECK qty >= 0`

**ledger_entries** (double-entry): id, txn_id, account_ref, asset, direction (DEBIT/CREDIT), amount, trade_id, created_at
Invariant: for every `txn_id`, sum of debits equals sum of credits.

**outbox**: id, aggregate_id, topic, key, payload, created_at, published_at (NULL until sent)

**processed_events**: consumer_name, event_id, processed_at, `PRIMARY KEY(consumer_name, event_id)`

### 6.3 Indexes

`orders(user_id, created_at DESC)`, `orders(symbol, status)`, `trades(symbol, executed_at DESC)`, partial index on `outbox WHERE published_at IS NULL`.

### 6.4 Read model

Candles and portfolio views are projections. Start with plain queries; add a materialized view or a dedicated projection table only if profiling shows the need.

---

## 7. Consistency Between Kafka and Postgres (the heart of the project)

### 7.1 Transactional Outbox (order-service)

Problem: "write order to DB" and "publish to Kafka" are two systems; one can fail.
Solution: in **one DB transaction**, insert the order, reserve funds, and insert an outbox row. A separate relay goroutine polls unpublished outbox rows (`SELECT ... FOR UPDATE SKIP LOCKED`), publishes to Kafka, then marks `published_at`. Crash between publish and mark means a duplicate publish, which is handled by 7.2.

### 7.2 Idempotent Consumer (ledger-service)

For each consumed event, within one transaction:
1. `INSERT INTO processed_events ... ON CONFLICT DO NOTHING`; if zero rows inserted, skip (already handled).
2. Apply balance, position and ledger changes.
3. Commit, then commit Kafka offset.

### 7.3 Fund reservation

Buy limit order: reserve `price * qty` from `available` to `reserved`. Sell order: reserve shares. On fill, move reserved to settled; on cancel or reject, release. This prevents spending the same cash twice.

### 7.4 Concurrency control

Settlement locks the involved account rows in a **consistent order** (by id) to avoid deadlocks. Document the isolation level used and why.

---

## 8. API Design

### 8.1 REST (JSON, versioned `/api/v1`)

| Method | Path | Purpose |
|---|---|---|
| POST | `/auth/register`, `/auth/login` | JWT access + refresh |
| GET | `/instruments` | List symbols |
| POST | `/orders` | Place order (requires `Idempotency-Key` header) |
| DELETE | `/orders/{id}` | Request cancel |
| GET | `/orders?status=&symbol=` | User's orders |
| GET | `/portfolio` | Balances, positions, unrealised P&L |
| GET | `/trades?symbol=` | Recent trades |
| GET | `/book/{symbol}?depth=20` | Order book snapshot |
| GET | `/candles/{symbol}?interval=1m&limit=` | OHLCV |

Errors: RFC 7807 problem JSON. Pagination: cursor-based.

### 8.2 WebSocket `/ws`

Client subscribes: `{"op":"subscribe","channels":["book:AAPL","trades:AAPL","user.orders"]}`.
Server pushes: `book.delta`, `trade`, `candle`, `order.update`. The gateway keeps a per-symbol book copy built from events, sends a snapshot on subscribe, then deltas with sequence numbers so the client can detect gaps and resync.
Backpressure: bounded per-client send buffer; drop slow clients rather than block the hub.

---

## 9. Frontend Design (React + TypeScript + Vite)

**Pages:** Login, Market (chart + order book + trade tape + order ticket), Portfolio, Order History, Admin/Metrics (optional).

**Key components:** `OrderBook` (depth bars, aggregated levels), `PriceChart` (TradingView `lightweight-charts`), `OrderTicket` (validation, optimistic UI), `PositionsTable`, `TradeTape`.

**State:**
- Server state: TanStack Query for REST.
- Live state: a single WebSocket client module feeding a store (Zustand), with reconnect + resubscribe + resync on sequence gap.
- Throttle UI renders (batch deltas per animation frame) so 1000 msgs/sec doesn't freeze the page.

**Testing:** Vitest + React Testing Library; Playwright for one end-to-end flow (register, place order, see fill).

---

## 10. Market Data Simulator

- Per symbol, generate prices via a random walk (geometric Brownian motion) with configurable drift and volatility.
- Optional **bot traders**: market-maker bots place bid/ask ladders around the simulated price so the book has liquidity; noise-trader bots send random orders. This keeps the demo alive and doubles as your load generator.
- Candle aggregator consumes `trades.executed` (and ticks when no trades) and emits OHLCV per interval.

---

## 11. Non-Functional Requirements

| Area | Target (single machine, docker compose) |
|---|---|
| Throughput | 5,000+ orders/sec end to end |
| Latency | Match p99 < 10 ms (engine); order-accepted-to-UI p99 < 200 ms |
| Durability | No acknowledged order lost across a service crash |
| Correctness | Ledger always balanced; no negative balances |
| Recovery | Matching engine restart to ready < 10 s |

Publish **measured** numbers in the README with hardware stated. Measured numbers on a resume beat claimed ones.

---

## 12. Observability

- **Logging:** Go `log/slog`, JSON, with `correlation_id` carried across HTTP and Kafka headers.
- **Metrics (Prometheus):** orders/sec, match latency histogram, consumer lag per group, outbox backlog, DB pool usage, WS connections.
- **Tracing:** OpenTelemetry spanning HTTP to Kafka to consumers (Jaeger).
- **Dashboards:** Grafana (provisioned in repo) with one "exchange health" board.

---

## 13. Security

- Argon2id/bcrypt password hashing, short-lived JWT access tokens + refresh rotation.
- Input validation on every endpoint; tick/lot size and max order size checks.
- Per-user rate limiting; `Idempotency-Key` on order placement.
- Secrets via env/.env (not committed); least-privilege DB roles per service.

---

## 14. Testing Strategy

| Level | What |
|---|---|
| Unit | Matching engine scenarios table (partial fills, multi-level sweeps, cancels, market orders on thin books) |
| Property-based | Random order streams must satisfy invariants in 5.5 (use Go fuzzing or `rapid`) |
| Integration | `testcontainers-go` with real Kafka + Postgres: order to trade to settled balances |
| Idempotency | Deliver every event twice; final DB state must be identical |
| Chaos | Kill matching engine mid-stream; restart; verify no lost or duplicated trades |
| Load | Bot-driven generator (or k6) with throughput and latency report |
| Frontend | Component tests + one Playwright E2E |

---

## 15. Repository Layout

```
orderflow/
  cmd/{gateway,order,matching,ledger,marketdata,botrunner}/
  internal/{events,kafka,db,engine,ledger,auth,config,obs}/
  migrations/
  proto/                (or schemas/)
  web/                  (React app)
  deploy/{docker-compose.yml,prometheus/,grafana/}
  docs/{adr/,architecture.md,benchmarks.md}
  Makefile
  README.md
```

Write short **ADRs** (architecture decision records) for: partition key choice, outbox vs Kafka transactions, order book data structure, snapshot strategy, money representation. Interviewers love these.

---

## 16. Milestones and Acceptance Criteria

Build in this order. Each milestone should be runnable and demoable before moving on.

**M0: Skeleton (1-2 days).** Monorepo, docker compose with Kafka (KRaft), Postgres, migrations, health endpoints, CI (lint + test).
*Done when:* `make up` starts everything; CI is green.

**M1: Matching engine core, no Kafka (3-5 days).** Pure Go package with order book + tests + property tests + benchmark.
*Done when:* all invariants pass on 1M random orders; benchmark recorded.

**M2: Orders to Kafka to engine (3-4 days).** order-service with outbox; engine consumes commands and emits events and trades.
*Done when:* an order posted over REST appears as a trade in `trades.executed`.

**M3: Ledger and settlement (3-4 days).** Idempotent consumer, double-entry, reservations.
*Done when:* duplicate-delivery test passes; ledger balances to zero.

**M4: Gateway and WebSocket (3 days).** Auth, REST, WS hub, snapshot + delta protocol.
*Done when:* wscat shows live book and trades.

**M5: React UI (5-7 days).** Market page, portfolio, order history.
*Done when:* a user can trade end to end from the browser.

**M6: Market data and bots (2-3 days).** Simulator, candles, liquidity bots.
*Done when:* the exchange looks alive with no human input.

**M7: Reliability (3-4 days).** Snapshots, crash recovery, chaos test, DLQ.
*Done when:* kill -9 on the engine loses no trades.

**M8: Observability and load test (2-3 days).** Metrics, dashboards, tracing, benchmark report.
*Done when:* README contains measured throughput/latency and a Grafana screenshot.

**M9: Polish (2 days).** Architecture diagram, ADRs, demo GIF/video, one-command setup, live demo link if possible.

**Stretch (pick one):** AI layer (anomaly/manipulation detection on the trade stream such as wash trading or spoofing, or an LLM-generated portfolio explainer), Kubernetes manifests, gRPC between services, a second asset class, CQRS read model for analytics.

---

## 17. Working With Antigravity

- Put this SDD at `docs/SDD.md` and keep it as the source of truth the agent reads.
- Give the agent **one milestone at a time**, with its acceptance criteria. Don't hand over the whole project at once.
- For M1 and M3, write the invariants and test cases yourself first, then let the agent implement against them. These are the parts you must be able to explain line by line in an interview.
- Review every diff. If you cannot explain why a design choice was made, ask the agent for the reasoning, then record it in an ADR.

---

## 18. Resume and Interview Material

**Resume bullets (fill with your real numbers):**
- Designed and built an event-driven paper-trading exchange in Go with Kafka and PostgreSQL, sustaining **X orders/sec** with p99 match latency of **Y ms**.
- Implemented a price-time-priority matching engine with deterministic replay and snapshot-based crash recovery; verified no trade loss under forced process kills.
- Ensured exactly-once settlement effects using the transactional outbox pattern and idempotent consumers; double-entry ledger audited to zero balance by automated tests.
- Built a real-time React/TypeScript trading dashboard (order book, candlestick charts, portfolio) over WebSockets with sequence-gap resync.

**Questions you should be ready for:**
1. Why partition by symbol? What happens if one symbol is hot?
2. How do you prevent double spending of cash?
3. What happens if the engine crashes after emitting a trade but before committing its offset?
4. Why outbox instead of writing to Kafka and Postgres directly?
5. Why not use Kafka transactions for everything?
6. How would you scale this to 100x?

---

## 19. Risks

| Risk | Mitigation |
|---|---|
| Scope creep | Hold to M0-M9; stretch goals only after M8 |
| Kafka local setup friction | Use KRaft single broker in compose; document it |
| UI flooding at high tick rates | Server-side conflation and client-side frame batching |
| Subtle ledger bugs | Property tests + reconciliation job that checks invariants nightly |
