# ADR-005: Money and Quantity Representation

- **Status:** Accepted
- **Date:** 2026-10-28

## Context

IEEE-754 floating-point numbers (`float64`) cannot represent base-10 decimals like `0.01` exactly, causing rounding drift that breaks double-entry ledger invariants ($\sum \text{DEBIT} = \sum \text{CREDIT}$).

## Decision

- All prices and monetary amounts in Go and PostgreSQL are stored as **`int64` (`BIGINT`) minor units** (cents: `18900` = `\$189.00`).
- All quantities are stored as **`int64` (`BIGINT`) whole shares/lots**.
- Formatting to/from decimal strings happens strictly at the UI / JSON boundary.

## Consequences

- **Pros:** Exact integer arithmetic in CPU registers; zero rounding errors; fast comparisons in the matching engine; native `BIGINT` constraints in PostgreSQL (`CHECK (available >= 0)`).
- **Cons:** API clients must agree on minor-unit scaling (`tick_size` on `instruments`).

