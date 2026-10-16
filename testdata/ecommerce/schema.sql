-- idxa e2e fixture: a small synthetic e-commerce schema.
--
-- Deliberately MISSING indexes (the advisor is expected to find these):
--   orders(customer_id)                              -- "my orders" lookups + FK
--   events(customer_id, event_type, occurred_at)     -- activity feed
--   products(sku)                                    -- non-unique SKU lookups
--   orders(status) partial WHERE status = 'pending'  -- skewed 5-value column
--
-- Load with:  psql "$DSN" -f testdata/ecommerce/schema.sql
-- Then:       cd testdata/ecommerce && go run ./gen -dsn "$DSN" -scale 1

DROP SCHEMA IF EXISTS shop CASCADE;
CREATE SCHEMA shop;
SET search_path = shop, public;

CREATE TABLE customers (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email         text        NOT NULL,
    full_name     text        NOT NULL,
    country_code  char(2)     NOT NULL,
    tier          text        NOT NULL DEFAULT 'standard',   -- standard | gold | platinum
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT customers_email_key UNIQUE (email)
);

CREATE TABLE addresses (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id   bigint      NOT NULL REFERENCES customers(id),
    kind          text        NOT NULL DEFAULT 'shipping',   -- shipping | billing
    line1         text        NOT NULL,
    city          text        NOT NULL,
    postal_code   text        NOT NULL,
    country_code  char(2)     NOT NULL,
    is_default    boolean     NOT NULL DEFAULT false
);
CREATE INDEX addresses_customer_id_idx ON addresses (customer_id);

CREATE TABLE categories (
    id            int         GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    parent_id     int         REFERENCES categories(id),
    name          text        NOT NULL,
    slug          text        NOT NULL,
    CONSTRAINT categories_slug_key UNIQUE (slug)
);

CREATE TABLE products (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    category_id   int         NOT NULL REFERENCES categories(id),
    sku           text        NOT NULL,          -- intentionally NOT indexed
    name          text        NOT NULL,
    description   text,
    price_cents   integer     NOT NULL CHECK (price_cents >= 0),
    active        boolean     NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX products_category_id_idx ON products (category_id);

CREATE TABLE inventory (
    product_id    bigint      NOT NULL REFERENCES products(id),
    warehouse     text        NOT NULL,
    quantity      integer     NOT NULL DEFAULT 0 CHECK (quantity >= 0),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (product_id, warehouse)
);

CREATE TABLE orders (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id   bigint      NOT NULL REFERENCES customers(id),   -- intentionally NOT indexed
    address_id    bigint      REFERENCES addresses(id),
    -- 5-value text status with a heavily skewed distribution:
    -- ~90% delivered, ~5% shipped, ~2% pending, ~2% cancelled, ~1% paid
    status        text        NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending', 'paid', 'shipped', 'delivered', 'cancelled')),
    total_cents   integer     NOT NULL DEFAULT 0,
    currency      char(3)     NOT NULL DEFAULT 'USD',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX orders_created_at_idx ON orders (created_at);

CREATE TABLE order_items (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id      bigint      NOT NULL REFERENCES orders(id),
    product_id    bigint      NOT NULL REFERENCES products(id),
    quantity      integer     NOT NULL CHECK (quantity > 0),
    unit_cents    integer     NOT NULL
);
CREATE INDEX order_items_order_id_idx   ON order_items (order_id);
CREATE INDEX order_items_product_id_idx ON order_items (product_id);

CREATE TABLE payments (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id      bigint      NOT NULL REFERENCES orders(id),
    provider      text        NOT NULL,                 -- card | wallet | bank
    provider_ref  text        NOT NULL,
    amount_cents  integer     NOT NULL,
    status        text        NOT NULL,                 -- authorized | captured | refunded | failed
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT payments_provider_ref_key UNIQUE (provider, provider_ref)
);
CREATE INDEX payments_order_id_idx ON payments (order_id);

CREATE TABLE shipments (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id      bigint      NOT NULL REFERENCES orders(id),
    carrier       text        NOT NULL,
    tracking_no   text        NOT NULL,
    shipped_at    timestamptz,
    delivered_at  timestamptz
);
CREATE INDEX shipments_order_id_idx ON shipments (order_id);
CREATE INDEX shipments_tracking_no_idx ON shipments (tracking_no);

-- Append-only activity log. Only the PK exists; the advisor should propose
-- (customer_id, event_type, occurred_at) for the activity-feed query.
CREATE TABLE events (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id   bigint      NOT NULL,        -- no FK on purpose: hot append path
    event_type    text        NOT NULL,        -- page_view | add_to_cart | checkout | search | login
    occurred_at   timestamptz NOT NULL,
    product_id    bigint,
    payload       jsonb
);
