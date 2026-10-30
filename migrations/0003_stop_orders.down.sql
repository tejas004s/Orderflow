ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_check;
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_type_check;
ALTER TABLE orders ADD CONSTRAINT orders_type_check CHECK (type IN ('LIMIT', 'MARKET'));
ALTER TABLE orders ADD CONSTRAINT orders_check CHECK ((type = 'LIMIT' AND price IS NOT NULL) OR (type = 'MARKET' AND price IS NULL));
ALTER TABLE orders DROP COLUMN IF EXISTS stop_price;
