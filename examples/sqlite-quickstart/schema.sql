CREATE TABLE users (
  id         INTEGER PRIMARY KEY,
  email      VARCHAR(120) NOT NULL UNIQUE,
  first_name VARCHAR(50) NOT NULL,
  last_name  VARCHAR(50) NOT NULL,
  city       VARCHAR(60),
  is_active  BOOLEAN NOT NULL,
  created_at TIMESTAMP NOT NULL
);

CREATE TABLE orders (
  id        INTEGER PRIMARY KEY,
  user_id   INTEGER NOT NULL REFERENCES users(id),
  status    VARCHAR(20) NOT NULL,
  total     DECIMAL(10,2) NOT NULL,
  placed_at TIMESTAMP NOT NULL
);
