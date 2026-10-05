-- scripts/seed-load.sql
--
-- Seed for the steady-load test (scripts/k6/load.js). Run after `make reset`.
--
-- 1,000 units with 100 seats each and 100 customers. IDs are deterministic so
-- k6 can build them without querying the database:
--   unit i     -> a0000000-0000-0000-0000-<i, 12 digits>
--   customer i -> c0000000-0000-0000-0000-<i, 12 digits>
--
-- Sizing: one run is ~50 bookings/s x (15 s warm-up + 120 s measured) plus 400
-- setup bookings, ~7,200 bookings spread round-robin, ~7 per unit. 100 seats
-- per unit means no unit sells out, so every POST stays on the 201 path.

CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

INSERT INTO customers (customer_id, name, email)
SELECT ('c0000000-0000-0000-0000-' || lpad(i::text, 12, '0'))::uuid,
       'Load Customer ' || i,
       'load-' || i || '@example.com'
FROM generate_series(1, 100) AS i
ON CONFLICT (customer_id) DO NOTHING;

INSERT INTO inventory_units (unit_id, name, available_units, total_units, currency, price_minor, min_book)
SELECT ('a0000000-0000-0000-0000-' || lpad(i::text, 12, '0'))::uuid,
       'Load Unit ' || i,
       100, 100, 'IDR', 50000000, 1
FROM generate_series(1, 1000) AS i
ON CONFLICT (unit_id) DO NOTHING;

ANALYZE customers;
ANALYZE inventory_units;
