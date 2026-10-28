# ADR-006: PostgreSQL Isolation Level and Deadlock-Free Lock Ordering

- **Status:** Accepted
- **Date:** 2026-10-28

## Context

When `ledger-service` settles a trade between user $A$ and user $B$, and `order-service` concurrently reserves funds for new orders from $A$ or $B$, concurrent transactions touch the same `accounts` and `positions` rows. Using `SERIALIZABLE` isolation under high contention causes frequent `40001` serialization failures and retry storms.

## Decision

- Use PostgreSQL's default **`READ COMMITTED`** isolation level combined with explicit pessimistic row locking (`SELECT ... FOR UPDATE`) and `CHECK (available >= 0)` / `CHECK (reserved >= 0)` constraints.
- Whenever a transaction locks multiple `accounts` or `positions` rows (such as buyer and seller settlement), it **sorts the target keys in ascending order (`user_id`, `symbol`/`currency`) before acquiring `FOR UPDATE` locks**.

## Consequences

- **Pros:** Provably free of lock-order deadlocks between concurrent settlements and reservations; no serialization retry loops.
- **Cons:** Developers must follow the canonical lock-ordering helper in `internal/ledger` and `internal/order`.

