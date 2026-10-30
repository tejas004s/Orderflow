ALTER TABLE orders ADD COLUMN IF NOT EXISTS stop_price BIGINT CHECK (stop_price > 0);

ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_type_check;
ALTER TABLE orders ADD CONSTRAINT orders_type_check CHECK (type IN ('LIMIT', 'MARKET', 'STOP', 'STOP_LIMIT'));

ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_check;
ALTER TABLE orders ADD CONSTRAINT orders_check CHECK (
    (type = 'LIMIT' AND price IS NOT NULL AND stop_price IS NULL) OR
    (type = 'MARKET' AND price IS NULL AND stop_price IS NULL) OR
    (type = 'STOP' AND price IS NULL AND stop_price IS NOT NULL) OR
    (type = 'STOP_LIMIT' AND price IS NOT NULL AND stop_price IS NOT NULL)
);
