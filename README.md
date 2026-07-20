<p align="center">
  <img src="assets/logo.png" alt="Seedora" width="200" height="200">
</p>

<h1 align="center">Seedora</h1>

<p align="center">
  Schema-aware database seeding with a UI. Point it at a database, pick a generator per column, get realistic rows.
</p>

<p align="center">
  <a href="#quick-start">Quick start</a> ·
  <a href="#how-it-works">How it works</a> ·
  <a href="#configuration">Configuration</a> ·
  <a href="#cli-reference">CLI</a> ·
  <a href="#contributing">Contributing</a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/go-1.22%2B-00ADD8?logo=go&logoColor=white" alt="Go 1.22+">
  <img src="https://img.shields.io/badge/databases-20%20engines-2f6f4e" alt="Databases">
  <img src="https://img.shields.io/badge/license-MIT-blue" alt="License">
</p>

---

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
# Homebrew
brew install seedora

# Install script
curl -sSL https://get.seedora.dev | sh

# From source
go install github.com/mrb/seedora/cmd/seedora@latest
```

Run:

```bash
seedora --dsn "postgres://user:pass@localhost:5432/myapp_dev"
```

Seedora opens the mapping UI on `http://localhost:7777`. Review the generators it proposed, set a row count per table, and click **Seed**.

Docker, if you prefer:

```bash
docker run --rm -p 7777:7777 ghcr.io/mrb/seedora:latest \
  --dsn "postgres://user:pass@host.docker.internal:5432/myapp_dev"
```

## How it works

### 1. Connect

Seedora accepts a standard connection string for any supported engine:

```
postgres://user:pass@localhost:5432/myapp_dev
mysql://user:pass@localhost:3306/myapp_dev
file:./dev.db
```

Credentials are read from the DSN, the `SEEDORA_DSN` environment variable, or entered in the UI. They are never written to the config file.

### 2. Scan

Seedora introspects the live schema: tables, columns, data types, nullability, defaults, primary keys, unique constraints, check constraints, enums, and foreign keys.

If you have no database yet, hand it a `.sql` file instead. Seedora parses the DDL, shows you the schema it found, and offers to apply it to a target database before seeding — useful for spinning up a scratch instance from a migration dump.

### 3. Map

Every column is assigned a generator, inferred from its name and type. The inference is deliberately conservative: a confident match is applied, an ambiguous one is flagged for you to confirm.

| Column | Detected type | Generator | Options |
| --- | --- | --- | --- |
| `users.first_name` | `varchar(50)` | First name | Locale `en_US` |
| `users.email` | `varchar(255)` | Email | Unique |
| `users.created_at` | `timestamptz` | Date between | `2023-01-01` → now |
| `users.is_active` | `boolean` | Weighted boolean | 85% true |
| `orders.user_id` | `integer` (FK) | Foreign key | References `users.id` |
| `orders.status` | `order_status` | Enum pick | `pending`, `paid`, `shipped` |
| `orders.total` | `numeric(10,2)` | Decimal range | `5.00` – `2500.00` |

Available generators cover names, emails, usernames, phone numbers, addresses, companies, URLs, lorem text, integers and decimals with ranges, dates and timestamps, weighted booleans, UUIDs, enum picks, sequential counters, regex patterns, values drawn from a list you paste in, and an explicit null option for nullable columns.

### 4. Seed

Tables are topologically sorted so parents are inserted before children, and child rows draw their foreign keys from the parent IDs actually written in the same run. Unique columns are backed by a collision-checked pool rather than retried at random, so a 100k-row unique email column does not degrade as it fills.

Everything runs inside a single transaction. A failure halfway through leaves the database exactly as it was.

## Configuration

Any mapping can be saved as `seedora.yaml` and committed alongside your code:

```yaml
version: 1

tables:
  users:
    rows: 10000
    columns:
      first_name: { generator: first_name, locale: en_US }
      email:      { generator: email, unique: true }
      is_active:  { generator: bool, true_weight: 0.85 }
      created_at: { generator: date_between, from: 2023-01-01, to: now }

  orders:
    rows: 100000
    columns:
      user_id: { generator: foreign_key, references: users.id }
      status:  { generator: enum, values: [pending, paid, shipped] }
      total:   { generator: decimal, min: 5.00, max: 2500.00 }
```

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
| `--port <n>` | UI port (default `7777`) |

## Performance

100,000 rows is the design target, not the ceiling. Generation runs across a worker pool sized to available cores, and writes go out as batched multi-row prepared statements — `COPY` on Postgres, extended inserts on MySQL. In practice this lands between 50,000 and 100,000 rows per second, depending on column count, generator mix, and how many indexes are live during the insert.

If you are seeding millions of rows, drop non-essential indexes first and rebuild afterwards; Seedora will suggest this when it detects an expensive index on a target table.

## Supported databases

Seedora targets the twenty engines developers actually seed. Each driver implements the same interface — introspect, plan, bulk-insert — so a table mapping written for one engine transfers to another with only type-specific generators changing.

**Relational — stable**

| Engine | Versions | Bulk path |
| --- | --- | --- |
| PostgreSQL | 12+ | `COPY` |
| MySQL | 8.0+ | Extended `INSERT` |
| MariaDB | 10.6+ | Extended `INSERT` |
| SQLite | 3.35+ | Batched transaction |
| Microsoft SQL Server | 2019+ | `BULK INSERT` |
| Oracle Database | 19c+ | Array binds |
| CockroachDB | 23.1+ | `COPY` |
| Amazon Aurora | Postgres & MySQL | Engine-native |
| Google Cloud Spanner | GoogleSQL dialect | Mutations API |
| YugabyteDB | 2.18+ | `COPY` |

**Analytical & embedded — stable**

| Engine | Versions | Bulk path |
| --- | --- | --- |
| ClickHouse | 23+ | Native block insert |
| DuckDB | 0.10+ | Appender API |
| Snowflake | Current | Staged `COPY INTO` |
| Google BigQuery | Current | Storage Write API |
| Amazon Redshift | Current | `COPY` from staged files |

**Non-relational — stable**

| Engine | Versions | Bulk path |
| --- | --- | --- |
| MongoDB | 6.0+ | `insertMany` |
| Redis | 7+ | Pipelined writes |
| Cassandra / ScyllaDB | 4.0+ / 5.0+ | Batched prepared statements |
| Elasticsearch / OpenSearch | 8+ / 2+ | `_bulk` API |
| Neo4j | 5+ | `UNWIND` batches |

Schema inference differs by family. Relational and analytical engines are introspected from the catalog, so mapping is fully automatic. Schemaless stores (MongoDB, Redis, Elasticsearch) are inferred by sampling existing documents where available, or defined from a JSON Schema or a document template you supply; Neo4j is mapped as node labels and relationship types rather than tables.

## Safety

Seedora is a development tool and defaults to assuming you did not mean to run it against production. It refuses to connect when the database name or host matches common production patterns unless you pass `--i-know-what-im-doing`. Truncation is opt-in per table and always surfaces a confirmation showing the row counts that will be destroyed. Credentials stay in the DSN or environment and are never persisted to `seedora.yaml`.

## Contributing

Issues and pull requests are welcome. To build locally:

```bash
git clone https://github.com/mrb/seedora
cd seedora
make dev     # runs the Go server and the UI in watch mode
make test    # unit tests plus integration tests against dockerised databases
make build   # single binary with the UI embedded
```

New generators live in `internal/generator` and only need to satisfy a small interface; adding one is usually a single file plus a test. New database engines implement the driver interface in `internal/db`.

## License

MIT — see [LICENSE](LICENSE).
