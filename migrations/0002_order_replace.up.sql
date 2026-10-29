-- Order replace (cancel-replace). See docs/adr/0007-order-replace.md.
--
-- A replace is two-phase: order-service reserves any EXTRA cash/shares up front
-- and records the command here; ledger-service resolves the row when the
-- matching engine answers with REPLACED (release the surplus, adopt the new
-- price/qty) or REPLACE_REJECT (give the extra reservation back).
CREATE TABLE order_replaces (
    command_id    UUID PRIMARY KEY,   -- REPLACE command id, echoed on the engine's reply
    order_id      UUID NOT NULL REFERENCES orders(id),
    user_id       UUID NOT NULL REFERENCES users(id),
    prev_price    BIGINT NOT NULL CHECK (prev_price > 0),
    prev_qty      BIGINT NOT NULL CHECK (prev_qty > 0),
    new_price     BIGINT NOT NULL CHECK (new_price > 0),
    new_qty       BIGINT NOT NULL CHECK (new_qty > 0),
    -- Cash (BUY, minor units) or shares (SELL) moved into reservation when the
    -- replace was accepted; the worst case over every fill the engine could
    -- have made before it processes the command.
    reserve_delta BIGINT NOT NULL DEFAULT 0 CHECK (reserve_delta >= 0),
    status        TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'APPLIED', 'REJECTED')),
    reject_reason TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at   TIMESTAMPTZ
);
-- At most one unresolved replace per order.
CREATE UNIQUE INDEX order_replaces_one_pending ON order_replaces (order_id) WHERE status = 'PENDING';
CREATE INDEX order_replaces_order ON order_replaces (order_id);

