# OrderFlow

An event-driven **paper-trading exchange** written in Go, built on Kafka and PostgreSQL, with a React/TypeScript dashboard.
Users trade virtual money. A price-time-priority matching engine pairs their orders, and an idempotent double-entry ledger settles every trade. The UI streams the order book, trades and the user's portfolio live over WebSockets.

> Simulation for learning and portfolio use. No real money, no broker connectivity.

The design is in [`docs/SDD.md`](docs/SDD.md), and the decisions behind it are recorded in [`docs/adr/`](docs/adr/).

## Architecture

```
React SPA <-> api-gateway (REST + WS) -> order-service --(outbox)--> Kafka
                    ^                                                  |
                    +----------- fan-out <-- orders.events / trades <--+-- matching-engine
                                                                       +-- ledger-service -> Postgres
                                                                       +-- marketdata-service
```

| Service | Port | Role |
|---|---|---|
| `gateway` | 8080 | Auth (JWT), REST `/api/v1`, WebSocket `/ws`, rate limiting |
| `order` | 8081 | Validate, reserve funds, persist order + outbox row in one transaction |
| `matching` | 8082 | In-memory books per symbol, price-time priority, deterministic replay |
| `ledger` | 8083 | Idempotent settlement into a double-entry ledger |
| `marketdata` | 8084 | GBM price simulator + 1m OHLCV candles |
| `botrunner` | 8085 | Market-maker / noise-trader bots (liquidity + load generator) |

Each service exposes `/healthz` (liveness), `/readyz` (dependency checks) and `/metrics` (Prometheus).

## Quick start

Requirements: Docker with Compose v2. Go 1.26+ is only needed if you want to run the tests locally.

```bash
# Linux / macOS / Windows (PowerShell or Git Bash) - from the repo root
docker compose -f deploy/docker-compose.yml up -d --build --wait
# or, with make:
make up
```

Check that everything is healthy:

```bash
docker compose -f deploy/docker-compose.yml ps
curl http://localhost:8080/readyz      # PowerShell: curl.exe http://localhost:8080/readyz
```

Stop with `make down`, or wipe all data with `make clean` (which runs `docker compose ... down -v`).

Kafka is reachable from the host at `localhost:29092` and Postgres at `localhost:5432` (`orderflow`/`orderflow`).

## Development

```bash
make test        # unit tests
make test-race   # with race detector
make test-int    # integration tests via testcontainers (needs Docker)
make bench       # matching engine benchmarks
make lint        # golangci-lint
```

## Repository layout

```
cmd/         one binary per service + ofctl (admin CLI: migrate, topics, health)
internal/    shared packages (app, config, db, kafka, obs, svc, engine, ...)
migrations/  SQL migrations (embedded into ofctl)
deploy/      docker-compose, Prometheus, Grafana
docs/        SDD, ADRs, benchmarks
web/         React app
```

## Status

| Milestone | Status |
|---|---|
| M0 Skeleton | ✅ |
| M1 Matching engine core | ✅ (~276k cmds/sec/core, 1M-order invariant suite passing) |
| M2 Orders → Kafka → engine | ⏳ |
| M3 Ledger & settlement | ⏳ |
| M4 Gateway & WebSocket | ⏳ |
| M5 React UI | ⏳ |
| M6 Market data & bots | ⏳ |
| M7 Reliability | ⏳ |
| M8 Observability & load test | ⏳ |
| M9 Polish | ⏳ |

