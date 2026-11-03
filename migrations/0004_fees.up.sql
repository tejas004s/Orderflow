-- Seed system EXCHANGE_FEES user and USD revenue account.
INSERT INTO users (id, email, password_hash, is_bot)
VALUES ('00000000-0000-0000-0000-00000000fee5', 'exchange_fees@system.orderflow', 'system', true)
ON CONFLICT (id) DO NOTHING;

INSERT INTO accounts (user_id, currency, available, reserved)
VALUES ('00000000-0000-0000-0000-00000000fee5', 'USD', 0, 0)
ON CONFLICT (user_id, currency) DO NOTHING;

ALTER TABLE trades ADD COLUMN IF NOT EXISTS maker_fee BIGINT NOT NULL DEFAULT 0;
ALTER TABLE trades ADD COLUMN IF NOT EXISTS taker_fee BIGINT NOT NULL DEFAULT 0;

