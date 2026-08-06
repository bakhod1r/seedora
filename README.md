<p align="center">
  <img src="assets/logo.png" alt="Seedora" width="200" height="200">
</p>

<h1 align="center">Seedora</h1>

<p align="center">
  Schema-aware database seeding with a UI. Point it at a database, pick a generator per column, get realistic rows.
</p>

<p align="center">
  <a href="https://bakhod1r.github.io/seedora"><strong>Live demo</strong></a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="#how-it-works">How it works</a> ·
  <a href="#configuration">Configuration</a> ·
  <a href="#cli-reference">CLI</a> ·
  <a href="#contributing">Contributing</a>
</p>

<p align="center">
  <img src="https://img.shields.io/github/v/release/bakhod1r/seedora?color=2f6f4e" alt="Release">
  <img src="https://img.shields.io/badge/go-1.26%2B-00ADD8?logo=go&logoColor=white" alt="Go 1.26+">
  <img src="https://img.shields.io/badge/engines-PostgreSQL%20%C2%B7%20MySQL%20%C2%B7%20SQLite-2f6f4e" alt="Databases">
  <img src="https://img.shields.io/badge/license-MIT-blue" alt="License">
</p>

---

> **Status: early.** PostgreSQL, SQLite, and MySQL/MariaDB work end to end —
> introspection, the mapping UI, `seedora.yaml`, and seeding at six figures of
> rows. The other engines listed under
> [Supported databases](#supported-databases) are designed for and not yet
> written; the table says which is which.

## Overview

Seedora fills empty databases with data that looks real. It connects to your database, introspects the schema, infers a sensible generator for every column, and presents the result as an editable mapping in a browser UI. You adjust what you disagree with, choose row counts, and run — foreign keys, unique constraints, and insertion order are handled for you.

The target is concrete: **an empty instance to 100,000 realistic rows in under two minutes, without writing SQL or application code.**

Seedora is a single Go binary with the UI embedded. There is no runtime to install, no `node_modules`, and nothing to add to your project's dependency tree.

## Why

Seeding is a problem most teams solve twice, badly. The first solution is a hand-written seed script, which drifts out of sync the moment someone adds a column and fails loudly in CI weeks later. The second is a sanitised production dump, which is slow to produce, awkward to share, and a compliance liability the day it leaks.

Seedora takes a third position: the schema is already a complete description of what the data should look like, so it should be the configuration. Column names and types carry most of the intent — `email` is an email, `created_at` is a timestamp, `user_id` references `users` — and Seedora reads that intent directly instead of asking you to restate it in code.

## Quick start

Install:

```bash
go install github.com/bakhod1r/seedora/cmd/seedora@latest
```

Run:

```bash
seedora --dsn "postgres://user:pass@localhost:5432/myapp_dev"
```

Seedora opens the mapping UI on `http://localhost:7777`. Review the generators it proposed, set a row count per table, and click **Seed**.

No database to hand? SQLite needs nothing installed:

```bash
make demo && make dev
```

Homebrew, an install script, and a container image are planned; for now `go install` and `make build` are the two ways to get a binary.

## How it works

### 1. Connect

Seedora accepts a standard connection string:

```
postgres://user:pass@localhost:5432/myapp_dev
mysql://user:pass@localhost:3306/myapp_dev
./dev.db
```

MySQL also accepts the form its own client library uses,
`user:pass@tcp(127.0.0.1:3306)/myapp_dev`, and `mariadb://` is the same driver.

Credentials are read from the DSN, the `SEEDORA_DSN` environment variable, a file named by `SEEDORA_DSN_FILE` (for a Docker or Kubernetes secret mount), a `.env` file, or the UI's connect screen.

They are never written to `seedora.yaml` — not by convention but by construction, since the format has nowhere to put one. A connection entered in the UI can be remembered for next time; that list lives under your user config directory with `0600` permissions, outside the repository, and the password is only stored if you tick the box that says so.

Variable expansion is deliberately not applied to the DSN. A password is not shell syntax, and expanding `$ecret123` would quietly hand the driver an empty string.

### 2. Scan

Seedora introspects the live schema: tables, columns, data types, nullability, defaults, primary keys, unique constraints, enums, and foreign keys. Views, generated columns, and partitions of a partitioned table are read but never seeded — none of them is a thing you insert rows into.

Dragging one column onto another points the first at the second — a mapping change, filled from the parent's keys at run time. Dragging a key onto another table's **card** asks which relationship it is — one-to-many, one-to-one, or many-to-many — and then adds what that answer needs: a foreign key column (`users.id` dropped on `orders` proposes `orders.user_id REFERENCES users(id)`), the same column with a unique constraint, or a join table holding one key from each side as its composite primary key, and drawing between two existing columns offers to add the database's own constraint. Both are schema changes and both go through the SQL dialog first; SQLite has no `ALTER TABLE ADD CONSTRAINT`, so there the constraint is refused with the reason and the mapping still works.

Reading a schema from `.sql` files instead of a live database works too, which is what `--migrations` is for:

```bash
seedora run --dsn "$DATABASE_URL" --migrations ./db/migrations --rows 500
```

Seedora reads the directory in filename order, replays the `CREATE TABLE`, `ALTER TABLE ADD/DROP COLUMN`, and `DROP TABLE` statements in it, and compares the result with the live database. Tables the database is missing are created; tables it already has are left alone. Down migrations are skipped — a separate `.down.sql` file, a `down/` directory, or the half of a file after `-- +goose Down` or `-- migrate:down`.

This is not a migration runner: it records nothing in your migration tool's table, runs nothing in reverse, and alters no table that already exists. It exists so a fresh checkout, or a branch that adds three tables, can be seeded without running the project's migration tool first.

In the UI, `--migrations` scans on load and draws the missing tables as drafts, so the SQL is reviewed in the same dialog every other schema edit goes through before it runs. **Import** does the same for a single `.sql` file you paste or drop in.

### 3. Map

Every column is assigned a generator, inferred from its name and type. The inference is deliberately conservative: a confident match is applied, an ambiguous one is flagged for you to confirm.

| Column | Detected type | Generator | Options |
| --- | --- | --- | --- |
| `users.first_name` | `varchar(50)` | `firstname` | Locale `en_US`, max 50 |
| `users.email` | `varchar(255)` | `email` | Unique |
| `users.created_at` | `timestamptz` | `time` | |
| `users.is_active` | `boolean` | `bool` | `true_weight: 0.85` |
| `users.city` | `varchar(60)` | `city` | `null_rate: 0.05` |
| `orders.user_id` | `bigint` (FK) | `foreign_key` | References `users.id` |
| `orders.status` | `order_status` | `enum` | Values read from the enum type |
| `orders.total` | `numeric(10,2)` | `amount` | `min: 0, max: 99999999` |
| `orders.id` | `bigserial` (PK) | `default` | Skipped — the database assigns it |

Available generators cover names, emails, usernames, phone numbers, addresses, companies, URLs, lorem text, integers and decimals with ranges, dates and timestamps, weighted booleans, UUIDs, enum picks, sequential counters, financial and vehicle identifiers, values drawn from a list you paste in, a fixed constant, and an explicit null option for nullable columns — around a hundred in the picker, drawn from [synth](https://github.com/bakhod1r/synth).

Inference is conservative about what it claims. A column named `email` is an email and is marked confident. A column named `name` gets a person's name because that is the common case, but it is flagged rather than trusted — on a `products` table it is wrong, and nothing in the catalog says which kind of table this is. `seedora scan` reports the count of flagged columns, and the UI puts a marker on each one.

The picker shows every generator, with the ones matching the column's type first. Nothing is hidden: a deliberate mismatch is a legitimate choice.

Click **Preview** on any table to generate a few rows and see what the current mapping actually produces. It writes nothing.

### 4. Seed

Tables are topologically sorted so parents are inserted before children, and child rows draw their foreign keys from the parent IDs actually written in the same run. Unique columns are backed by a collision-checked pool rather than retried at random: a collision is repaired from the row's index, which no other row shares, so a 100k-row unique email column does not degrade as it fills.

Values are clamped to what the column will actually hold, so a `varchar(30)` never receives 31 characters and a `numeric(10,2)` never overflows its precision. A generator knows what an email looks like; only the catalog knows how wide this particular column is.

Everything runs inside a single transaction. A failure halfway through — or a Ctrl-C — leaves the database exactly as it was.

## Configuration

Any mapping can be saved as `seedora.yaml` and committed alongside your code:

```yaml
version: 1
locale: en_US

tables:
  users:
    rows: 10000
    columns:
      id:         { generator: default, skip: true }
      email:      { generator: email, unique: true, max: 255 }
      first_name: { generator: firstname, max: 50 }
      city:       { generator: city, max: 60, null_rate: 0.05 }
      is_active:  { generator: bool, true_weight: 0.85 }
      created_at: { generator: time }

  orders:
    rows: 100000
    columns:
      id:      { generator: default, skip: true }
      user_id: { generator: foreign_key, references: users.id }
      status:  { generator: enum, values: [pending, paid, shipped] }
      total:   { generator: amount, min: 5, max: 2500 }
```

`seedora scan` writes exactly this file, so the fastest way to see the format is to run it against your own schema.

Replay it anywhere — a teammate's machine, a CI job, an ephemeral preview environment:

```bash
seedora run --config seedora.yaml --dsn "$DATABASE_URL"
```

Re-scanning a database that already has a config preserves your overrides and prompts only for columns it has not seen before. Pass `--seed 42` to make generated data deterministic across runs, which is what you want when tests assert on specific rows.

## CLI reference

| Command | Description |
| --- | --- |
| `seedora` | Start the UI and open the connect screen |
| `seedora run --config <file>` | Run a saved config headlessly, no UI |
| `seedora scan --dsn <dsn> -o <file>` | Introspect a schema and write a starter config |
| `seedora validate --config <file>` | Check a config against the live schema |
| `seedora version` | Print version and build info |

Common flags:

| Flag | Description |
| --- | --- |
| `--dsn <string>` | Connection string; falls back to `SEEDORA_DSN` |
| `--rows <n>` | Override the row count for every table |
| `--seed <n>` | Fix the random seed for reproducible output |
| `--truncate` | Truncate target tables before seeding |
| `--dry-run` | Generate and validate without writing |
| `--migrations <path>` | Migration directory or `.sql` file; tables in it the database lacks are created first |
| `--port <n>` | UI port (default `7777`) |
| `--batch <n>` | Rows generated per unit of work (default `5000`) |
| `--locale <name>` | Generator locale (default `en_US`) |
| `--quiet` | Suppress the progress line |
| `--i-know-what-im-doing` | Bypass the production-target guard |

Every flag has an environment variable — `SEEDORA_DSN`, `SEEDORA_ROWS`, `SEEDORA_SEED`, and so on. `seedora --help` prints the full list.

In the UI, **Import YAML** loads a `seedora.yaml` into the running session and **Export** downloads the current mapping. An import is merged against the schema you are connected to: choices for columns that still exist are kept, new columns get a proposal, and columns the database no longer has are dropped. A config that does not fit is reported in full and not applied.

## Performance

The design rule is that the database should never wait on Seedora.

Generation runs on a worker pool sized to the machine, ahead of the writer, feeding rows into a single bulk write per table — one `COPY` for the whole table on Postgres, however many rows it holds. The server parses one statement and then only takes bytes off the wire. Finished batches are re-ordered by index before they are written, so the parallelism costs nothing in reproducibility: the same `--seed` produces byte-identical data whatever the workers happened to do.

Measured on an M-series laptop, two tables, nine and five columns, one foreign key, one unique email column:

| | rows/second |
| --- | --- |
| Generation alone (`--dry-run`) | ~2,000,000 |
| PostgreSQL 16 via `COPY`, over Docker's network stack | ~244,000 |
| SQLite, pure-Go driver | ~24,000 |

Generation is roughly an order of magnitude faster than the fastest database can accept rows, which is the intended shape: the bottleneck should be the engine, not us.

Clicking a relationship's cardinality marker asks what it should be — one-to-many or one-to-one — and changes it. One-to-one is a mapping change first: the seeder gives each parent exactly one child, and a run asking for more children than there are parents is refused with the arithmetic. It then offers the unique index that makes the database enforce it.

A join table seeds like any other: its key pairs are assigned from the parent pools rather than drawn, so they are distinct by construction and spread over the parents, and a run asking for more rows than there are possible pairs is refused before it writes anything.

Dropping non-essential indexes before a large run and rebuilding afterwards still helps, and Seedora does not yet detect or suggest it.

## Supported databases

Each driver implements the same interface — introspect, plan, bulk-insert — so a table mapping written for one engine transfers to another with only type-specific generators changing. Three engines are implemented, and the third is what proved the interface was drawn right rather than fitted to two.

**Working**

| Engine | Versions | Bulk path |
| --- | --- | --- |
| PostgreSQL | 12+ | `COPY` |
| MySQL / MariaDB | 8.0+ / 10.5+ | Prepared chunked multi-row `INSERT` |
| SQLite | 3.35+ | Prepared chunked `INSERT` |

The Postgres driver also accepts `cockroachdb://`, `yugabyte://`, and Aurora Postgres DSNs, since they speak the same wire protocol — but only Postgres itself has been tested.

Two things about MySQL are worth knowing before you point Seedora at one. Schema changes made in the diagram cannot be rolled back: MySQL commits the open transaction before every DDL statement, so a batch that fails part way leaves the earlier statements applied, and the review dialog says so. And truncation is `DELETE` rather than `TRUNCATE`, because `TRUNCATE` is DDL there and would commit the transaction that a failed run is meant to unwind — so emptying a table that another table's rows still point at is refused, and the error names the constraint.

**Designed, not yet written**

Relational: SQL Server, Oracle, CockroachDB, Aurora, Cloud Spanner, YugabyteDB.
Analytical: ClickHouse, DuckDB, Snowflake, BigQuery, Redshift.
Non-relational: MongoDB, Redis, Cassandra/ScyllaDB, Elasticsearch/OpenSearch, Neo4j.

Schema inference will differ by family. Relational and analytical engines are introspected from the catalog, so mapping is automatic. Schemaless stores would be inferred by sampling existing documents, or defined from a JSON Schema or a document template; Neo4j maps to node labels and relationship types rather than tables.

Adding an engine means implementing `db.Driver` and `db.Tx` in a package under `internal/db` and importing it from `cmd/seedora`. Nothing above the driver layer branches on which database it is pointed at.

## Safety

Seedora is a development tool and defaults to assuming you did not mean to run it against production.

It refuses to connect when the database name contains `prod`, `production`, `live`, `master`, or `primary` at a word boundary, or when the host looks like a managed instance — RDS, Neon, Supabase, PlanetScale, Cockroach Cloud, Timescale. Any non-local host is refused. `--i-know-what-im-doing` bypasses the check, and it is spelled out in full so nobody types it without reading it.

Local targets are never guarded. That is deliberate: a guard that fires on `localhost` would train people to pass the bypass flag by reflex, which would defeat it everywhere it matters.

The guard is a speed bump, not a permission system. It is a heuristic over a connection string, and it cannot know what your database is for.

Truncation is opt-in, and the UI shows the row count that will be destroyed before it runs. Credentials stay in the DSN, the environment, or the per-machine connection store, and are never written to `seedora.yaml`.

## Contributing

Issues and pull requests are welcome. To build locally:

```bash
git clone https://github.com/bakhod1r/seedora
cd seedora
make demo    # builds a throwaway SQLite database under tmp/
make dev     # runs the UI against it
make test    # unit tests
make race    # unit tests under the race detector
make build   # single binary with the UI embedded
```

The UI is embedded with `go:embed` and has no build step, no bundler, and no `node_modules` — editing `internal/ui/assets/` and rebuilding is the whole loop.

Generation runs on a worker pool, so `make race` is not optional for changes under `internal/seed`.

New database engines implement `db.Driver` and `db.Tx` in a package under `internal/db`, and are enabled by importing that package from `cmd/seedora`. Generators come from [synth](https://github.com/bakhod1r/synth); a new one belongs upstream, and is picked up here by adding it to the catalogue in `internal/ui/generators.go`.

## Built on

- [synth](https://github.com/bakhod1r/synth) — the generation engine: name-based inference, ~130 generators, 52 locales, referential and temporal coherence.
- [oneenv](https://github.com/bakhod1r/oneenv) — configuration and credentials: the `.env` cascade, secret files, and the `Secret[T]` wrapper that keeps the DSN out of logs.
- [pgx](https://github.com/jackc/pgx) — the PostgreSQL driver and its `COPY` implementation.
- [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) — SQLite in pure Go, so the binary needs no cgo.

## License

MIT — see [LICENSE](LICENSE).
