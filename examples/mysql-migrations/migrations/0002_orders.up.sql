CREATE TABLE orders (
  id         BIGINT AUTO_INCREMENT PRIMARY KEY,
  user_id    BIGINT         NOT NULL REFERENCES users(id),
  status     ENUM('pending','paid','shipped','cancelled') NOT NULL,
  total      DECIMAL(10,2)  NOT NULL,
  placed_at  DATETIME       NOT NULL
);

-- Ignored rather than fatal: Seedora reads the statements it can act on and
-- steps over the rest, which is what makes it usable on real migration files.
CREATE INDEX orders_user_idx ON orders (user_id);
