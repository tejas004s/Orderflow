# ADR-007: Order Replace (Cancel-Replace / Amend Price & Quantity)

- **Status:** Accepted
- **Date:** 2026-10-29

## Context

Traders often need to adjust the price or quantity of a resting limit order without losing their place in the queue if the adjustment is minor, or to avoid round-trip cancel + place latency.

A standard exchange rule (used by Nasdaq, CME, LSE) governs queue priority on replace:
1. **Reducing quantity at the same price:** Retains time priority within the price level FIFO queue.
2. **Increasing quantity or modifying price:** Forfeits time priority. If price is modified, it acts as an atomic cancel + new limit order (potentially trading immediately as a taker if crossing the opposite side).

In an asynchronous distributed architecture (Order Service -> Outbox -> Kafka -> Matching Engine -> Ledger):
- The matching engine may have already partially or fully executed the resting order before the `REPLACE` command reaches it.
- Additional cash or shares must be pre-reserved during the initial HTTP request to prevent overdrafts / double-spending, while any surplus reservation must be safely refunded by the ledger when the engine's response (`REPLACED` or `REPLACE_REJECT`) is settled.

## Decision

1. **Exchange Rules in `internal/engine`:**
   - Reducing `Qty` at same `Price`: calls `b.shrink(o, newQty)` in place, preserving doubly-linked list pointers and time priority.
   - Increasing `Qty` at same `Price`: unlinks and appends to the tail of the level (`b.rest(o)`), losing priority.
   - Changing `Price`: unlinks from old price level, moves to new price, evaluates crossing against opposite side (matching as taker if crossing, with self-trade prevention cancelling the replaced order if crossing own order), and rests any remaining quantity.
   - Emits `EvReplaced` carrying `PrevPrice`, `PrevQty`, `PrevFilled`, or `EvReplaceReject` if invalid/unknown/closed.

2. **End-to-End Pipeline & Two-Phase Reservation:**
   - **`PATCH /api/v1/orders/{order_id}`** in `internal/order/http.go` and `internal/gateway/gateway.go`.
   - `order.Service.ReplaceOrder`:
     - Checks user ownership and ensures order is open (`NEW`, `ACCEPTED`, `PARTIALLY_FILLED`).
     - Computes worst-case delta reservation across all possible execution states before the engine processes the command.
     - Atomically locks account/position and locks additional funds/shares if delta > 0.
     - Inserts tracking row into `order_replaces` (`PENDING`).
     - Emits `REPLACE` to Kafka `orders.commands` via transactional outbox.
   - `ledger.Service`:
     - On `REPLACED`: adopts new price/qty, calculates actual reservation adjustment based on `(PrevPrice, PrevQty, PrevFilled)`, resolves `order_replaces` to `APPLIED`, and refunds any surplus reservation to user `available`.
     - On `REPLACE_REJECT`: restores previous quantity, marks `order_replaces` as `REJECTED`, and refunds any extra reservation back to user `available`.

## Consequences

- **Pros:**
  - Strict preservation of price-time priority according to institutional exchange rules.
  - No race conditions or overdrafts between concurrent trades and replaces.
  - Atomicity guaranteed via the Transactional Outbox pattern and double-entry ledger reconciliation.
  - Zero lock contention in the matching engine.
- **Cons:**
  - Upfront reservation uses conservative worst-case delta until the matching engine confirms the execution state.
