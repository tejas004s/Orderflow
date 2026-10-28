# ADR-002: Transactional Outbox and Idempotent Consumers vs. Kafka Transactions

- **Status:** Accepted
- **Date:** 2026-10-28

## Context

`order-service` must atomically reserve user funds in PostgreSQL and publish a command to `orders.commands`. Similarly, `ledger-service` consumes `trades.executed` / `orders.events` from Kafka and settles balances in PostgreSQL. Neither dual-writes nor Kafka transactions alone can span both PostgreSQL and Kafka without two-phase commit (XA).

## Decision

1. **Transactional Outbox (`order-service`):** Placing an order executes a single PostgreSQL transaction that inserts the `orders` row, moves cash/shares from `available` to `reserved`, and inserts a row into `outbox`. A background relay polls `outbox WHERE published_at IS NULL FOR UPDATE SKIP LOCKED`, publishes to Kafka with `acks=all` + idempotent producer, and stamps `published_at`.
2. **Deterministic Engine Deduplication (`matching-engine`):** If the outbox relay crashes after publishing to Kafka but before setting `published_at`, it will republish the command. The engine keeps a deterministic sliding window of seen `order_id`s (persisted in snapshots) and emits `EvDuplicate` without mutating the book.
3. **Idempotent Consumer (`ledger-service`):** Every side-effecting consumer begins its PostgreSQL transaction with `INSERT INTO processed_events (consumer_name, event_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`. If zero rows are inserted, the transaction rolls back and the offset is committed. Offsets are committed manually **only after** the DB transaction commits.

## Consequences

- **Pros:** Exactly-once *effects* across PostgreSQL and Kafka using standard `READ COMMITTED` transactions; resilient to process crashes at any instruction boundary.
- **Cons:** Slight additional latency from outbox polling (mitigated via short poll intervals / notification channel) and extra storage for `outbox` and `processed_events`.

