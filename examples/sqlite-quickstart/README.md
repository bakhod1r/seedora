# SQLite in three commands

No configuration, no server, nothing to install. SQLite is a file, so the
"database" here is one that does not exist yet.

```sh
sqlite3 shop.db < schema.sql
seedora --dsn ./shop.db
```

The UI opens on the schema. Every column already carries a proposed generator;
the ones Seedora is unsure about are marked **to check**. Adjust what you
disagree with, press **Seed**, and the rows are written.

To skip the UI entirely once a mapping exists:

```sh
seedora scan --dsn ./shop.db          # write a starter seedora.yaml
seedora run  --dsn ./shop.db --rows 5000
```

## What to look at

- `users.email` is `UNIQUE`, so Seedora generates it uniquely rather than
  hoping. Ask for more rows than the generator can produce distinctly and it
  says so before writing anything.
- `orders.user_id` is a foreign key, so it draws from the ids `users` actually
  received — and `users` is inserted first because of it.
- `orders.id` is left to the database. A serial key that Seedora filled in
  would leave the sequence behind the rows.
