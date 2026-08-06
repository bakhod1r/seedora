-- The first migration: people, and the country they are in.
--
-- Nothing here is Seedora-specific. This is an ordinary golang-migrate
-- directory — numbered files, one direction per file — of the sort every
-- project already has.

CREATE TABLE countries (
  id   BIGINT AUTO_INCREMENT PRIMARY KEY,
  code VARCHAR(2)  NOT NULL UNIQUE,
  name VARCHAR(80) NOT NULL
);

CREATE TABLE users (
  id         BIGINT AUTO_INCREMENT PRIMARY KEY,
  email      VARCHAR(120) NOT NULL UNIQUE,
  first_name VARCHAR(50)  NOT NULL,
  last_name  VARCHAR(50)  NOT NULL,
  phone      VARCHAR(30),
  country_id BIGINT       REFERENCES countries(id),
  is_active  BOOLEAN      NOT NULL,
  created_at DATETIME     NOT NULL
);
