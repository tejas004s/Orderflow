-- OrderFlow initial schema (SDD section 6).
-- Money is stored as BIGINT minor units (cents); quantities as BIGINT shares.

CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email         TEXT NOT NULL UNIQUE CHECK (email = lower(email)),
    password_hash TEXT NOT NULL,
    is_bot        BOOLEAN NOT NULL DEFAULT false,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE refresh_tokens (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    family_id  UUID NOT NULL,
    token_hash BYTEA NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX refresh_tokens_family ON refresh_tokens (family_id);

CREATE TABLE instruments (
    symbol        TEXT PRIMARY KEY CHECK (symbol ~ '^[A-Z]{1,10}$'),
    name          TEXT NOT NULL,
    tick_size     BIGINT NOT NULL CHECK (tick_size > 0),     -- minor units
    lot_size      BIGINT NOT NULL CHECK (lot_size > 0),      -- shares
    max_order_qty BIGINT NOT NULL CHECK (max_order_qty > 0),
    ref_price     BIGINT NOT NULL CHECK (ref_price > 0),     -- seed / fallback price
    status        TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'HALTED'))
);

CREATE TABLE accounts (
    id        BIGSERIAL PRIMARY KEY,
    user_id   UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    currency  TEXT NOT NULL DEFAULT 'USD',
    available BIGINT NOT NULL DEFAULT 0 CHECK (available >= 0),
    reserved  BIGINT NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    UNIQUE (user_id, currency)
);

CREATE TABLE positions (
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    symbol       TEXT NOT NULL REFERENCES instruments(symbol),
    qty          BIGINT NOT NULL DEFAULT 0 CHECK (qty >= 0),          -- free shares
    reserved_qty BIGINT NOT NULL DEFAULT 0 CHECK (reserved_qty >= 0), -- locked by open sells
    avg_cost     BIGINT NOT NULL DEFAULT 0 CHECK (avg_cost >= 0),     -- minor units per share
    PRIMARY KEY (user_id, symbol)
);

CREATE TABLE orders (
    id               UUID PRIMARY KEY,
    user_id          UUID NOT NULL REFERENCES users(id),
    symbol           TEXT NOT NULL REFERENCES instruments(symbol),
    side             TEXT NOT NULL CHECK (side IN ('BUY', 'SELL')),
    type             TEXT NOT NULL CHECK (type IN ('LIMIT', 'MARKET')),
    price            BIGINT CHECK (price > 0),
    protection_price BIGINT CHECK (protection_price > 0),
    qty              BIGINT NOT NULL CHECK (qty > 0),
    filled_qty       BIGINT NOT NULL DEFAULT 0 CHECK (filled_qty >= 0 AND filled_qty <= qty),
    -- Cash (BUY, minor units) or shares (SELL) still reserved for this order.
    reserved_amount  BIGINT NOT NULL DEFAULT 0 CHECK (reserved_amount >= 0),
    status           TEXT NOT NULL CHECK (status IN
                        ('NEW', 'ACCEPTED', 'PARTIALLY_FILLED', 'FILLED', 'CANCELLED', 'REJECTED')),
    reject_reason    TEXT,
    cancel_requested_at TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((type = 'LIMIT' AND price IS NOT NULL) OR (type = 'MARKET' AND price IS NULL))
);
CREATE INDEX orders_user_created ON orders (user_id, created_at DESC);
CREATE INDEX orders_symbol_status ON orders (symbol, status);

CREATE TABLE trades (
    id             BIGSERIAL PRIMARY KEY,
    match_id       UUID NOT NULL UNIQUE,  -- deterministic id from the engine
    symbol         TEXT NOT NULL REFERENCES instruments(symbol),
    price          BIGINT NOT NULL CHECK (price > 0),
    qty            BIGINT NOT NULL CHECK (qty > 0),
    buy_order_id   UUID NOT NULL REFERENCES orders(id),
    sell_order_id  UUID NOT NULL REFERENCES orders(id),
    buyer_id       UUID NOT NULL REFERENCES users(id),
    seller_id      UUID NOT NULL REFERENCES users(id),
    aggressor_side TEXT NOT NULL CHECK (aggressor_side IN ('BUY', 'SELL')),
    executed_at    TIMESTAMPTZ NOT NULL
);
CREATE INDEX trades_symbol_executed ON trades (symbol, executed_at DESC);

-- Double-entry ledger. For every (txn_id, asset): sum(DEBIT) = sum(CREDIT).
-- DEBIT increases the balance of account_ref, CREDIT decreases it.
CREATE TABLE ledger_entries (
    id          BIGSERIAL PRIMARY KEY,
    txn_id      UUID NOT NULL,
    account_ref TEXT NOT NULL,   -- e.g. 'user:<uuid>' or 'system:equity'
    asset       TEXT NOT NULL,   -- 'USD' or an instrument symbol
    direction   TEXT NOT NULL CHECK (direction IN ('DEBIT', 'CREDIT')),
    amount      BIGINT NOT NULL CHECK (amount > 0),
    match_id    UUID REFERENCES trades(match_id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ledger_entries_txn ON ledger_entries (txn_id);
CREATE INDEX ledger_entries_account ON ledger_entries (account_ref, asset);

CREATE TABLE outbox (
    id           BIGSERIAL PRIMARY KEY,
    aggregate_id TEXT NOT NULL,
    topic        TEXT NOT NULL,
    key          BYTEA NOT NULL,
    payload      BYTEA NOT NULL,
    headers      JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ
);
CREATE INDEX outbox_unpublished ON outbox (id) WHERE published_at IS NULL;

CREATE TABLE processed_events (
    consumer_name TEXT NOT NULL,
    event_id      UUID NOT NULL,
    processed_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer_name, event_id)
);

CREATE TABLE idempotency_keys (
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key          TEXT NOT NULL,
    request_hash BYTEA NOT NULL,
    order_id     UUID NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key)
);

CREATE TABLE engine_snapshots (
    partition    INT PRIMARY KEY,
    next_offset  BIGINT NOT NULL,   -- first offset NOT included in the snapshot
    snapshot     BYTEA NOT NULL,
    taken_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE candles (
    symbol       TEXT NOT NULL REFERENCES instruments(symbol),
    interval     TEXT NOT NULL,
    bucket_start TIMESTAMPTZ NOT NULL,
    open         BIGINT NOT NULL,
    high         BIGINT NOT NULL,
    low          BIGINT NOT NULL,
    close        BIGINT NOT NULL,
    volume       BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (symbol, interval, bucket_start)
);

INSERT INTO instruments (symbol, name, tick_size, lot_size, max_order_qty, ref_price) VALUES
    ('AAPL',  'Apple Inc.',            1, 1, 100000, 18900),
    ('MSFT',  'Microsoft Corp.',       1, 1, 100000, 41500),
    ('NVDA',  'NVIDIA Corp.',          1, 1, 100000, 12000),
    ('AMZN',  'Amazon.com Inc.',       1, 1, 100000, 18500),
    ('GOOGL', 'Alphabet Inc.',         1, 1, 100000, 16500),
    ('TSLA',  'Tesla Inc.',            1, 1, 100000, 24000),
    ('META',  'Meta Platforms Inc.',   1, 1, 100000, 50000),
    ('JPM',   'JPMorgan Chase & Co.',  1, 1, 100000, 20500);

