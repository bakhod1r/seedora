-- A twenty-table schema for looking at the diagram with something realistic in
-- it. A two-table demo proves the seeder works; it proves nothing about the
-- layout, the edge routing, or the colour scheme, which is what this is for.
--
-- The shape is an ordinary commerce application: people, a catalogue, orders
-- laid over both, and the support and audit tables that grow around them.
--
-- All four cardinalities are present on purpose, because each is drawn
-- differently: one to many everywhere, one to one at user_profiles, many to
-- many at product_tags, and a self reference at categories. Two tables point at
-- the same parent (orders has a billing and a shipping address), which is where
-- an edge router that assumes one arrow per pair draws on top of itself.

CREATE TABLE countries (
  id         INTEGER PRIMARY KEY,
  code       VARCHAR(2) NOT NULL UNIQUE,
  name       VARCHAR(80) NOT NULL
);

CREATE TABLE users (
  id         INTEGER PRIMARY KEY,
  email      VARCHAR(120) NOT NULL UNIQUE,
  first_name VARCHAR(50) NOT NULL,
  last_name  VARCHAR(50) NOT NULL,
  phone      VARCHAR(30),
  country_id INTEGER REFERENCES countries(id),
  is_active  BOOLEAN NOT NULL,
  created_at TIMESTAMP NOT NULL
);

CREATE TABLE addresses (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id),
  country_id INTEGER NOT NULL REFERENCES countries(id),
  line1      VARCHAR(120) NOT NULL,
  line2      VARCHAR(120),
  city       VARCHAR(60) NOT NULL,
  postcode   VARCHAR(16) NOT NULL
);

CREATE TABLE suppliers (
  id         INTEGER PRIMARY KEY,
  name       VARCHAR(120) NOT NULL,
  email      VARCHAR(120) NOT NULL,
  country_id INTEGER REFERENCES countries(id)
);

CREATE TABLE warehouses (
  id         INTEGER PRIMARY KEY,
  name       VARCHAR(80) NOT NULL,
  city       VARCHAR(60) NOT NULL,
  country_id INTEGER NOT NULL REFERENCES countries(id)
);

-- A self reference: a category can sit under another category.
CREATE TABLE categories (
  id         INTEGER PRIMARY KEY,
  parent_id  INTEGER REFERENCES categories(id),
  name       VARCHAR(80) NOT NULL,
  slug       VARCHAR(80) NOT NULL UNIQUE
);

CREATE TABLE products (
  id          INTEGER PRIMARY KEY,
  category_id INTEGER NOT NULL REFERENCES categories(id),
  supplier_id INTEGER REFERENCES suppliers(id),
  sku         VARCHAR(32) NOT NULL UNIQUE,
  name        VARCHAR(120) NOT NULL,
  description TEXT,
  price       DECIMAL(10,2) NOT NULL,
  weight_kg   REAL,
  created_at  TIMESTAMP NOT NULL
);

CREATE TABLE product_variants (
  id         INTEGER PRIMARY KEY,
  product_id INTEGER NOT NULL REFERENCES products(id),
  sku        VARCHAR(32) NOT NULL UNIQUE,
  colour     VARCHAR(30),
  size       VARCHAR(20),
  price      DECIMAL(10,2) NOT NULL
);

CREATE TABLE inventory (
  id           INTEGER PRIMARY KEY,
  variant_id   INTEGER NOT NULL REFERENCES product_variants(id),
  warehouse_id INTEGER NOT NULL REFERENCES warehouses(id),
  quantity     INTEGER NOT NULL,
  updated_at   TIMESTAMP NOT NULL
);

CREATE TABLE carts (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id),
  created_at TIMESTAMP NOT NULL
);

CREATE TABLE cart_items (
  id         INTEGER PRIMARY KEY,
  cart_id    INTEGER NOT NULL REFERENCES carts(id),
  variant_id INTEGER NOT NULL REFERENCES product_variants(id),
  quantity   INTEGER NOT NULL
);

CREATE TABLE coupons (
  id           INTEGER PRIMARY KEY,
  code         VARCHAR(24) NOT NULL UNIQUE,
  percent_off  INTEGER NOT NULL,
  expires_at   TIMESTAMP
);

-- Two foreign keys into the same parent, which is where an edge router that
-- assumes one arrow per pair starts drawing on top of itself.
CREATE TABLE orders (
  id                  INTEGER PRIMARY KEY,
  user_id             INTEGER NOT NULL REFERENCES users(id),
  billing_address_id  INTEGER NOT NULL REFERENCES addresses(id),
  shipping_address_id INTEGER NOT NULL REFERENCES addresses(id),
  coupon_id           INTEGER REFERENCES coupons(id),
  status              VARCHAR(20) NOT NULL,
  total               DECIMAL(10,2) NOT NULL,
  placed_at           TIMESTAMP NOT NULL
);

CREATE TABLE order_items (
  id         INTEGER PRIMARY KEY,
  order_id   INTEGER NOT NULL REFERENCES orders(id),
  variant_id INTEGER NOT NULL REFERENCES product_variants(id),
  quantity   INTEGER NOT NULL,
  unit_price DECIMAL(10,2) NOT NULL
);

CREATE TABLE payments (
  id         INTEGER PRIMARY KEY,
  order_id   INTEGER NOT NULL REFERENCES orders(id),
  method     VARCHAR(20) NOT NULL,
  amount     DECIMAL(10,2) NOT NULL,
  paid_at    TIMESTAMP
);

CREATE TABLE shipments (
  id           INTEGER PRIMARY KEY,
  order_id     INTEGER NOT NULL REFERENCES orders(id),
  warehouse_id INTEGER NOT NULL REFERENCES warehouses(id),
  tracking     VARCHAR(40),
  shipped_at   TIMESTAMP
);

CREATE TABLE reviews (
  id         INTEGER PRIMARY KEY,
  product_id INTEGER NOT NULL REFERENCES products(id),
  user_id    INTEGER NOT NULL REFERENCES users(id),
  rating     INTEGER NOT NULL,
  body       TEXT,
  created_at TIMESTAMP NOT NULL
);

CREATE TABLE support_tickets (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id),
  order_id   INTEGER REFERENCES orders(id),
  subject    VARCHAR(140) NOT NULL,
  status     VARCHAR(20) NOT NULL,
  opened_at  TIMESTAMP NOT NULL
);

CREATE TABLE ticket_messages (
  id         INTEGER PRIMARY KEY,
  ticket_id  INTEGER NOT NULL REFERENCES support_tickets(id),
  user_id    INTEGER NOT NULL REFERENCES users(id),
  body       TEXT NOT NULL,
  sent_at    TIMESTAMP NOT NULL
);

-- One to one: the profile's key is also its foreign key, so a user has at most
-- one of these.
CREATE TABLE user_profiles (
  user_id    INTEGER PRIMARY KEY REFERENCES users(id),
  bio        TEXT,
  avatar_url VARCHAR(200),
  locale     VARCHAR(10) NOT NULL
);

CREATE TABLE tags (
  id   INTEGER PRIMARY KEY,
  name VARCHAR(40) NOT NULL UNIQUE
);

-- Many to many: two foreign keys and nothing else, which is what makes this a
-- pairing rather than a table in its own right.
CREATE TABLE product_tags (
  product_id INTEGER NOT NULL REFERENCES products(id),
  tag_id     INTEGER NOT NULL REFERENCES tags(id),
  PRIMARY KEY (product_id, tag_id)
);

CREATE TABLE audit_log (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER REFERENCES users(id),
  action     VARCHAR(40) NOT NULL,
  entity     VARCHAR(40) NOT NULL,
  entity_id  INTEGER NOT NULL,
  at         TIMESTAMP NOT NULL
);
