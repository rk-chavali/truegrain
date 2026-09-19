-- Fixture for the PostgreSQL metadata reader.
--
-- Separate from seed.sql, which the executor parity suite depends on and which
-- declares no constraints at all. This schema exists to be read, not queried:
-- nothing here inserts a row, because the reader never selects one.
--
-- Every table is shaped to catch a specific way a reader goes wrong. The
-- comments say which.

DROP SCHEMA IF EXISTS truegrain_introspect CASCADE;
CREATE SCHEMA truegrain_introspect;
DROP SCHEMA IF EXISTS truegrain_elsewhere CASCADE;
CREATE SCHEMA truegrain_elsewhere;

-- A composite type, to prove a structured column is named and left out rather
-- than emitted as a String that no filter can match.
CREATE TYPE truegrain_introspect.postal_address AS (
  line_one TEXT,
  city     TEXT,
  postcode TEXT
);

-- Single-column key, comments on both the table and a column, and a column
-- whose description contains the punctuation that breaks naive YAML emission.
CREATE TABLE truegrain_introspect.customers (
  customer_id INTEGER PRIMARY KEY,
  region      TEXT,
  signup_date DATE,
  email       TEXT NOT NULL,
  tags        TEXT[],
  address     truegrain_introspect.postal_address
);
COMMENT ON TABLE truegrain_introspect.customers
  IS 'One row per customer: the "canonical" list';
COMMENT ON COLUMN truegrain_introspect.customers.region
  IS 'Sales region, as the CRM spells it';

-- A compound primary key. Its order is (order_id, line_number), and a reader
-- that returns them the other way round produces a different file on every
-- run even though the fan-out check still passes.
CREATE TABLE truegrain_introspect.order_lines (
  order_id    INTEGER,
  line_number INTEGER,
  sku         TEXT,
  line_total  NUMERIC(12,2),
  PRIMARY KEY (order_id, line_number)
);

-- The trap this fixture exists for.
--
-- The compound foreign key below references order_lines (order_id,
-- line_number), but the referencing columns are declared (ship_order,
-- ship_line) while the table declares ship_line FIRST. So attnum order and
-- constraint order disagree.
--
-- A reader that pairs the two sides by column position in the table, or that
-- joins the catalogue on constraint name and sorts by attnum, emits
-- ship_line -> order_id and ship_order -> line_number. That join runs, returns
-- rows, and is wrong. Reading conkey and confkey in step cannot produce it.
CREATE TABLE truegrain_introspect.shipments (
  shipment_id INTEGER PRIMARY KEY,
  ship_line   INTEGER,
  ship_order  INTEGER,
  shipped_on  DATE,
  CONSTRAINT shipment_line
    FOREIGN KEY (ship_order, ship_line)
    REFERENCES truegrain_introspect.order_lines (order_id, line_number)
);

-- A single-column foreign key, so the compound path is not the only one
-- covered, plus a table that carries two foreign keys at once.
CREATE TABLE truegrain_introspect.orders (
  order_id    INTEGER PRIMARY KEY,
  customer_id INTEGER,
  order_date  DATE,
  order_total NUMERIC(12,2),
  CONSTRAINT orders_customer
    FOREIGN KEY (customer_id) REFERENCES truegrain_introspect.customers (customer_id)
);

-- No primary key at all. Its grain cannot be derived, and the reader must say
-- so rather than guess: a guessed key passes the fan-out check and inflates a
-- sum with complete confidence.
CREATE TABLE truegrain_introspect.events (
  occurred_at TIMESTAMPTZ,
  kind        TEXT,
  payload     TEXT
);

-- A view. Included because a view is a perfectly good thing to model, and it
-- carries no constraints, so it must arrive keyless rather than be skipped.
CREATE VIEW truegrain_introspect.recent_orders AS
  SELECT order_id, customer_id, order_date FROM truegrain_introspect.orders;

-- A foreign key pointing out of the schema being read. There is no dataset in
-- the generated model to join to, so this relationship must be dropped rather
-- than emitted naming a table that does not exist.
CREATE TABLE truegrain_elsewhere.warehouses (
  warehouse_id INTEGER PRIMARY KEY,
  name         TEXT
);
ALTER TABLE truegrain_introspect.shipments
  ADD COLUMN warehouse_id INTEGER,
  ADD CONSTRAINT shipment_warehouse
    FOREIGN KEY (warehouse_id) REFERENCES truegrain_elsewhere.warehouses (warehouse_id);
