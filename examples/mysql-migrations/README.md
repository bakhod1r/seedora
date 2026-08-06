# MySQL, from a migrations directory

The state this example is about: the schema lives in the repository as `.sql`
files, the database is empty, and there is nothing to introspect. That is a
fresh checkout, a new container, and every branch that adds a table.

`migrations/` is an ordinary golang-migrate directory — numbered files, one
direction per file. Nothing in it is Seedora-specific, and Seedora does not
write to it.

## Run it

Start a throwaway MySQL:

```bash
docker run -d --name seedora-mysql \
  -e MYSQL_ROOT_PASSWORD=seedora -e MYSQL_DATABASE=seedora_dev \
  -p 13306:3306 mysql:8
```

Create the missing tables and fill them, in one command:

```bash
seedora run \
  --dsn "mysql://root:seedora@127.0.0.1:13306/seedora_dev" \
  --migrations ./examples/mysql-migrations/migrations \
  --config ./examples/mysql-migrations/seedora.yaml \
  --seed 7
```

```
Read 3 migration file(s) from ./examples/mysql-migrations/migrations · 3 table(s), 0 already in the database
Created 3 table(s): countries, users, orders
Seeding MySQL · mysql://root:****@127.0.0.1:13306/seedora_dev
  [1/3] countries                50 / 50 (100%)
  [2/3] users                    2,000 / 2,000 (100%)
  [3/3] orders                   6,000 / 6,000 (100%)

Seeded 8,050 rows in 248ms · 32,422 rows/s
Seed 7 — pass --seed 7 to reproduce this exactly.
```

The row counts come from `seedora.yaml`, and `countries` is 50 on purpose:
`code` is a unique `VARCHAR(2)`, so there are only so many values that fit, and
asking for a thousand rows fails before it writes any of them rather than
writing duplicates.

Run it again and the second line becomes `3 already in the database`: existing
tables are never altered, only seeded.

To look at it rather than run it, drop `run` and the UI opens with the missing
tables drawn as drafts and the SQL in a dialog, unapplied:

```bash
seedora --dsn "mysql://root:seedora@127.0.0.1:13306/seedora_dev" \
        --migrations ./examples/mysql-migrations/migrations
```

## What is read, and what is not

| In the directory | What Seedora does with it |
| --- | --- |
| `CREATE TABLE` | Creates it, if the database does not have it |
| `ALTER TABLE … ADD COLUMN` / `DROP COLUMN` | Replays it onto the table above |
| `DROP TABLE` | Removes it from the replay — the table is not created |
| `CREATE INDEX`, grants, data backfills | Ignored, not an error |
| `*.down.sql`, `down/`, anything after `-- +goose Down` | Skipped |

Files are read in filename order, which is what the numbering is for.

This is not a migration runner. It records nothing in `schema_migrations`, runs
nothing in reverse, and alters no table that already exists — run your own tool
for that. It exists so an empty database can be seeded without one.

## The two MySQL things worth knowing

`0003_city.up.sql` alters a table the first migration created. The `users` table
Seedora creates has `city`, because the files are replayed rather than read one
at a time.

And `orders.status` is a MySQL `ENUM`. MySQL declares enums inline instead of as
a named type, so Seedora synthesises one per column and the mapping offers the
four labels — which is why `status` fills with `pending`/`paid`/`shipped`/
`cancelled` rather than random words.
