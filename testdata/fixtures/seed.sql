-- Fixture data for the retail model, loaded into DuckDB by `make demo`.
--
-- The line values are chosen so that line_revenue and order_revenue agree
-- exactly (885.50 both ways) while the line counts per order differ. That makes
-- the fan-out failure visible: joining orders to order_lines and summing
-- order_total naively gives 2361.00, which is wrong by a factor that varies per
-- order and therefore looks plausible.

CREATE SCHEMA IF NOT EXISTS main;

DROP TABLE IF EXISTS main.order_lines;
DROP TABLE IF EXISTS main.orders;
DROP TABLE IF EXISTS main.customers;

CREATE TABLE main.customers (
  customer_id  INTEGER PRIMARY KEY,
  region       VARCHAR,
  signup_date  DATE,
  email        VARCHAR
);

INSERT INTO main.customers VALUES
  (1, 'NE', DATE '2025-06-01', 'alice@northeast.example'),
  (2, 'MW', DATE '2025-07-15', 'bob@midwest.example'),
  (3, 'NE', DATE '2025-09-10', 'carol@northeast.example');

CREATE TABLE main.orders (
  order_id     INTEGER PRIMARY KEY,
  customer_id  INTEGER,
  order_date   DATE,
  status       VARCHAR,
  order_total  DECIMAL(12,2)
);

INSERT INTO main.orders VALUES
  (1, 1, DATE '2026-01-15', 'shipped',   100.00),
  (2, 1, DATE '2026-02-20', 'delivered', 250.00),
  (3, 2, DATE '2026-02-25', 'shipped',    75.50),
  (4, 3, DATE '2026-03-05', 'placed',    400.00),
  (5, 2, DATE '2026-03-30', 'cancelled',  60.00);

CREATE TABLE main.order_lines (
  order_id     INTEGER,
  line_number  INTEGER,
  item_id      VARCHAR,
  quantity     INTEGER,
  line_amount  DECIMAL(12,2),
  PRIMARY KEY (order_id, line_number)
);

INSERT INTO main.order_lines VALUES
  -- order 1: three lines
  (1, 1, 'SKU-A', 1,  30.00),
  (1, 2, 'SKU-B', 2,  40.00),
  (1, 3, 'SKU-C', 1,  30.00),
  -- order 2: one line
  (2, 1, 'SKU-A', 5, 250.00),
  -- order 3: two lines
  (3, 1, 'SKU-B', 1,  50.00),
  (3, 2, 'SKU-D', 3,  25.50),
  -- order 4: four lines
  (4, 1, 'SKU-A', 2, 100.00),
  (4, 2, 'SKU-B', 2, 100.00),
  (4, 3, 'SKU-C', 2, 100.00),
  (4, 4, 'SKU-D', 2, 100.00),
  -- order 5: one line
  (5, 1, 'SKU-C', 1,  60.00);

-- Marketing's campaigns, used by the two-team workspace fixture in
-- testdata/workspace. One campaign per order, which is what lets the planner
-- prove that attributing revenue by channel does not repeat an order row.
DROP TABLE IF EXISTS main.campaigns;

CREATE TABLE main.campaigns (
  campaign_id  INTEGER PRIMARY KEY,
  order_id     INTEGER UNIQUE,
  channel      VARCHAR,
  spend        DECIMAL(12,2)
);

INSERT INTO main.campaigns VALUES
  (10, 1, 'search',   12.00),
  (11, 2, 'social',   40.00),
  (12, 3, 'search',    9.00),
  (13, 4, 'email',     5.00),
  (14, 5, 'referral',  0.00);

-- Pre-aggregated tables, for the rollup routing tests.
--
-- Both hold the same 885.50 and the same 400.00 largest order as the fact
-- tables. A rollup that disagreed with its source would make every routing
-- test pass for the wrong reason: the numbers would differ and the test
-- would be measuring which table answered rather than whether the answer
-- was right.
--
-- largest_order is stored as a MAX and must be re-aggregated with MAX.
-- Combining these two rows with SUM gives 475.50, which is the class of
-- mistake the derived re-aggregation exists to prevent.

DROP TABLE IF EXISTS main.rollup_orders_by_region;
DROP TABLE IF EXISTS main.rollup_orders_by_region_month;
DROP TABLE IF EXISTS main.rollup_orders_stale;

CREATE TABLE main.rollup_orders_by_region (
  region        VARCHAR,
  order_revenue DECIMAL(12,2),
  largest_order DECIMAL(12,2),
  built_at      TIMESTAMP
);

INSERT INTO main.rollup_orders_by_region VALUES
  ('NE', 750.00, 400.00, NOW()),
  ('MW', 135.50,  75.50, NOW());

CREATE TABLE main.rollup_orders_by_region_month (
  region        VARCHAR,
  order_month   DATE,
  order_revenue DECIMAL(12,2),
  largest_order DECIMAL(12,2),
  built_at      TIMESTAMP
);

INSERT INTO main.rollup_orders_by_region_month VALUES
  ('NE', DATE '2026-01-01', 100.00, 100.00, NOW()),
  ('NE', DATE '2026-02-01', 250.00, 250.00, NOW()),
  ('NE', DATE '2026-03-01', 400.00, 400.00, NOW()),
  ('MW', DATE '2026-02-01',  75.50,  75.50, NOW()),
  ('MW', DATE '2026-03-01',  60.00,  60.00, NOW());

-- Stale on purpose, and wrong on purpose. Nothing should ever be answered
-- from this table, and the numbers in it are nowhere near the real ones so
-- that a test which accidentally reads it fails loudly rather than
-- agreeing by coincidence.
CREATE TABLE main.rollup_orders_stale (
  status        VARCHAR,
  order_revenue DECIMAL(12,2),
  built_at      TIMESTAMP
);
INSERT INTO main.rollup_orders_stale VALUES
  ('shipped',   1.00, TIMESTAMP '2020-01-01 00:00:00'),
  ('delivered', 2.00, TIMESTAMP '2020-01-01 00:00:00'),
  ('placed',    3.00, TIMESTAMP '2020-01-01 00:00:00'),
  ('cancelled', 4.00, TIMESTAMP '2020-01-01 00:00:00');
