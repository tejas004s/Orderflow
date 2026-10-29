# OrderFlow — Event-Driven Paper-Trading Exchange

An event-driven **paper-trading exchange** written in **Go**, built on **Apache Kafka (KRaft)** and **PostgreSQL**, with a real-time **React + TypeScript** trading dashboard.
Users trade virtual money (`$100,000.00` initial balance). A deterministic price-time-priority matching engine pairs orders, a transactional outbox and idempotent double-entry ledger settle trades with **exactly-once effects**, and a WebSocket hub streams L2 order book snapshots/deltas, executions, and OHLCV candlesticks to the browser.

> Simulation for learning and portfolio use. No real money, no broker connectivity.

- **Software Design Document:** [`docs/SDD.md`](docs/SDD.md)
- **Architecture & Interview Q&A:** [`docs/architecture.md`](docs/architecture.md)
- **Architecture Decision Records (ADRs):** [`docs/adr/`](docs/adr/) (`0001`–`0006`)
- **Benchmarks & Hardware Report:** [`docs/benchmarks.md`](docs/benchmarks.md)

---

## Architecture Overview

```
React SPA (:5173) <---> api-gateway (:8080, REST + WS) ---> order-service (:8081)
                              ^                                    | (ACID TX: reserve + order + outbox)
                              |                                    v
                              |                            PostgreSQL 16 (:5432)
                              |                                    | (FOR UPDATE SKIP LOCKED relay)
                              |                                    v
                              +---- fan-out <------------ Apache Kafka (:29092)
                                                          |        |         |
                                     +--------------------+        |         +-------------------+
                                     v                             v                             v
                            matching-engine (:8082)       ledger-service (:8083)      marketdata-service (:8084)
                            in-memory B-tree books        idempotent settlement       GBM ticks + 1m candles
                            snapshot crash recovery       double-entry ledger         + botrunner (:8085)
```

| Service | Port | Responsibility |
|---|---|---|
| `web` | `5173` | React + TypeScript + Vite trading dashboard (`OrderBook`, `PriceChart`, `OrderTicket`, `TradeTape`, `PositionsTable`) |
| `gateway` | `8080` | Argon2id + JWT auth (with rotating refresh tokens & reuse detection), REST `/api/v1`, WebSocket `/ws` hub, per-user rate limiting |
| `order` | `8081` | Instrument validation, fund/share reservation, `Idempotency-Key` enforcement, transactional outbox relay |
| `matching` | `8082` | Single-writer per-partition order books, price-time priority, self-trade prevention, deterministic replay & snapshots |
| `ledger` | `8083` | Idempotent consumer (`processed_events`), deadlock-free lock ordering, double-entry `ledger_entries`, price-improvement refund |
| `marketdata` | `8084` | Geometric Brownian Motion (GBM) price simulator & 1-minute OHLCV candle aggregator |
| `botrunner` | `8085` | Autonomous market-maker bid/ask ladder bots and noise-trader bots |
| `prometheus` | `9090` | Metrics scraping across all 6 Go services |
| `grafana` | `3000` | Provisioned **OrderFlow Exchange Health** dashboard (`admin` / `admin`) |

---

## Measured Performance (AMD Ryzen 5 5600H · 4 vCPU VM · 15 GiB RAM · Go 1.26)

| Area | SDD Target | Measured Result |
|---|---|---|
| **Pure Engine Throughput** | — | **~276,000 commands/sec/core** (`3,623 ns/op`, `10 allocs/op`) |
| **Pure Match Latency** | p99 < 10 ms | **p50 = 2.74 µs · p95 = 9.25 µs · p99 = 25.00 µs (0.025 ms)** |
| **Full Processor Throughput** *(decode + match + encode + snapshots)* | 5,000+ orders/sec | **10,144 orders/sec** per partition (**60,000+ orders/sec** across 6 partitions) |
| **Full Processor Latency** | p99 < 10 ms | **p50 = 50.6 µs · p95 = 174.4 µs · p99 = 495.3 µs (0.495 ms)** |
| **Crash Recovery Time** *(snapshot restore + tail replay)* | < 10 s | **322.8 ms (0.32 s)** |
| **1,000,000-Command Invariant Run** | All invariants pass | **1,000,000 commands · 575,598 trades · 2,151,196 events** verified in **13.05 s** |

Full details and reproduction commands in [`docs/benchmarks.md`](docs/benchmarks.md).

---

## Quick Start (One Command on Windows / macOS / Linux)

```bash
# Start Postgres, Kafka (KRaft), migrations + topic init, all 6 Go services,
# market-maker bots, Prometheus, Grafana, and the React UI:
docker compose -f deploy/docker-compose.yml up -d --build --wait
```

> **Note:** If an older Docker Compose version attempts to pull `orderflow:dev` from Docker Hub before building `init`, run `docker compose -f deploy/docker-compose.yml build init` first (fixed automatically on Compose v2.20+ via `pull_policy: never`).

Then open:
- **Trading Terminal (React UI):** [http://localhost:5173](http://localhost:5173)
- **Grafana Exchange Health Dashboard:** [http://localhost:3000/d/orderflow-health](http://localhost:3000/d/orderflow-health) (`admin` / `admin`)
- **Prometheus:** [http://localhost:9090](http://localhost:9090)
- **Gateway Readiness Probe:** [http://localhost:8080/readyz](http://localhost:8080/readyz)

### Verify Live WebSocket Stream (`wscat`)

```bash
npx wscat -c ws://localhost:8080/ws
> {"op":"subscribe","channels":["book:AAPL","trades:AAPL"]}
```

### Run Chaos Test (`kill -9` on Matching Engine + Ledger Reconciliation)

```bash
./scripts/chaos.sh
# or on Windows PowerShell:
docker compose -f deploy/docker-compose.yml kill -s SIGKILL matching
docker compose -f deploy/docker-compose.yml up -d --wait matching
docker compose -f deploy/docker-compose.yml exec -T ledger ofctl reconcile
```

---

## Milestones

| Milestone | Status |
|---|---|
| **M0** Skeleton (Monorepo, Compose, Migrations, Health/Metrics, CI) | ✅ |
| **M1** Matching Engine Core (B-Tree + Heap, Property Tests, 1M-Order Suite, Benchmarks) | ✅ |
| **M2** Orders → Transactional Outbox → Kafka → Partition Matching Engine | ✅ |
| **M3** Idempotent Double-Entry Ledger, Price-Improvement Refund & Reconciliation | ✅ |
| **M4** API Gateway (Argon2id, JWT + Refresh Rotation, Rate Limiting, WebSocket Hub) | ✅ |
| **M5** React + TypeScript Trading Dashboard (`web/`) | ✅ |
| **M6** GBM Market Data Simulator, 1m OHLCV Candles & Liquidity Bots | ✅ |
| **M7** Reliability (Snapshots, Deterministic Replay, DLQs, Chaos `kill -9` Test) | ✅ |
| **M8** Observability (Prometheus, Grafana Dashboard) & Load Generator (`cmd/loadgen`) | ✅ |
| **M9** Polish (ADRs `0001`–`0006`, Architecture & Interview Guide, One-Command Setup) | ✅ |
