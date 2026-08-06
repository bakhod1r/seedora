-- A later migration altering an earlier table. Seedora replays this, so the
-- table it creates has `city` — the schema as the last migration leaves it,
-- not as the first one wrote it.
ALTER TABLE users ADD COLUMN city VARCHAR(60);
