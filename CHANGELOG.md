# Changelog

Notable changes, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org/spec/v2.0.0.html).

Until `1.0.0`, a minor version may change `seedora.yaml` in a way that needs an
edit. Anything that does is called out under **Changed** with what to do about
it.

## [Unreleased]

## [0.5.0] — 2026-08-13

### Added

- **Three more engines tested, and seventeen more written.** CockroachDB,
  YugabyteDB and TiDB now run the end-to-end suite in CI alongside PostgreSQL,
  MySQL and SQLite: introspect a real catalog, seed it, read the rows back,
  check that unique columns are unique and foreign keys point at rows that
  exist. They needed no driver code of their own — they speak a wire protocol
  Seedora already drives — but they did need two fixes to the Postgres driver,
  which is the argument for testing rather than assuming. Seventeen further
  drivers are written and compile but have not been run against a live server:
  SQL Server, Oracle, SAP HANA, Vertica and Firebird; ClickHouse, Trino,
  Redshift and Databricks; Snowflake and BigQuery; and MongoDB, Elasticsearch,
  Cassandra, DynamoDB, Neo4j and Redis. The README separates the two claims and
  does not blur them into "supported".
- **Build tags, because one binary cannot hold every driver.** Linking all of
  them produces a 109.4 MB download, most of it three cloud SDKs arriving with
  gRPC, protobuf and a generated API surface for a whole platform. The engine
  set is now chosen at build time: default 21.0 MB (PostgreSQL, MySQL, SQLite,
  SQL Server, ClickHouse), `-tags enterprise` 37.7 MB, `-tags nosql` 33.7 MB,
  `-tags warehouse` 66.8 MB, `-tags all` 109.4 MB. Releases ship the default
  build for all five targets and the tagged builds for linux/amd64 and
  darwin/arm64. Sizes are measured, not estimated.
- **An honest transaction contract for engines that cannot honour it.**
  ClickHouse, Trino, Databricks, Snowflake, BigQuery, Cassandra, Elasticsearch,
  DynamoDB and Redis commit as they write. Their `Rollback` returns an error
  naming exactly what is already permanent rather than returning nil as though
  it had undone something, and each driver documents at the point of use what a
  mid-run failure leaves behind. Snowflake and BigQuery open no transaction at
  all, since one covering half a run is worse than none. Redshift can roll back
  and does.

### Fixed

- **Introspection returned no tables on CockroachDB.** The column query
  filtered on `NOT c.relispartition`, and CockroachDB reports that column as
  NULL — `NOT NULL` is NULL, so every table was dropped from the result and
  seeding failed on an empty schema.
- **Truncation failed on YugabyteDB whenever a plan covered a parent and its
  child.** Yugabyte cannot truncate the same relation twice inside one
  transaction, and Seedora truncated the child in its own right and then again
  through its parent's `CASCADE`. Truncates are now a single batched statement,
  which is also one round trip instead of several on every engine.

### Removed

- **DuckDB is dropped rather than deferred.** Its Go driver bundles a C++
  library across 25 cgo files, so linking it means `CGO_ENABLED=1` and a
  cross-compiler for each release target. The single binary is worth more, and
  saying no is better than leaving it on a roadmap as though it were coming.

## [0.4.0] — 2026-08-12

### Added

- **`seedora dump`, writing generated rows to files.** The same run — same
  plan, same insertion order, same foreign keys and uniqueness — with only the
  destination changed: one file per table, in `csv`, `json`, or `sql`.
  `internal/export` is a `db.Driver` and a `db.Tx` rather than a second code
  path, so a fixture is the data the seeder would have written. The schema
  still comes from a real database; nothing is written to it and the
  transaction is rolled back. Children point at rows in the fixture rather
  than at rows in a database the fixture will never be loaded into, a primary
  key the database would have assigned is assigned here instead (single-column
  integer keys only), and a `DECIMAL(10,2)` is written with two decimal places
  rather than a binary float's worth. Nothing reaches disk before commit, so a
  failed run leaves no partial fixture.
- **`--append`, for topping up a table that already has rows.** A run assumes
  its tables are empty and a second one fails on the first unique column it
  collides with. With `--append` nothing is emptied — including tables the
  plan asks to truncate, since that setting describes a run starting from
  empty — and every unique column is read back before the first row is
  generated, so the new rows are unique against the old. An integer collision
  counts on from the largest existing value. Refused with `--truncate`, whose
  meaning is the opposite, and on a join table, whose uniqueness is over the
  pair of foreign keys and cannot be read back one column at a time. Covered
  against SQLite, PostgreSQL and MySQL.
- **Undo and redo for plan edits**, on the toolbar and on `Cmd-Z` /
  `Cmd-Shift-Z`, over a bounded history of plan snapshots. Every plan edit
  already commits through `pushPlan`, so that is where a snapshot is taken and
  no individual edit has to remember. Both directions go through the server
  rather than reverting locally, a commit the server refuses records nothing,
  and the keystroke is ignored while a text field has focus. Connecting to
  another database clears the history. Scope is plan edits: schema changes,
  layout, and a committed run are each excluded — an applied `DROP TABLE` is
  not this tool's to reverse.
- **Clicking a key column lights the table it references**, scrolling it into
  view if it was off the board. A column with no reference lights its own
  card; a second click puts the canvas back.
- Motion for the edit controls, matching the rest of the page and listed in
  the reduced-motion switch that was already there.
- `make pg-up`, `make pg-down`, and `make test-all`, the Postgres counterparts
  to the MySQL pair plus one target that runs everything against both.

### Security

- **The UI answers only requests from Seedora's own page.** It holds a live
  database connection and has no password, and binding to loopback does not
  make it private: any page in the same browser can `POST` to
  `127.0.0.1:7777`, and a domain resolved to `127.0.0.1` is treated by the
  browser as that domain's own origin, which an `Origin` check alone cannot
  distinguish. Two checks now run on every route — `Sec-Fetch-Site` and
  `Origin` for where the request came from, `Host` for whether it was
  addressed to Seedora rather than to a rebound name. Reads are covered as
  well as writes: `/api/state` carries the schema and `/api/connections`
  carries every DSN this machine has connected to. Requests with neither
  header are allowed — `curl`, CI, the tests — since no web page can produce
  one. Binding past loopback with `--host` relaxes the `Host` check alone.

### Fixed

- **Two ways a 23-table schema failed to seed**, neither of which appears in a
  two-table one. A self-reference poisoned the foreign-key pool: an empty pool
  read before its own table had rows was cached, and every later child of that
  table got a cache hit on it. An empty pool is no longer cached. And a
  primary key that is also a foreign key was not seen as a one-to-one, because
  `Column.Unique` is read from the unique indexes and a single-column primary
  key does not always have one — on SQLite an `INTEGER PRIMARY KEY` is the
  rowid and `pragma_index_list` reports nothing. Being the sole primary key
  now counts as unique.
- **Folded cards no longer overlap.** A card's position was computed once and
  then kept, so it was still the position worked out for the height the card
  had at the time. Folding or unfolding changes that height, and the diagram
  ended up with cards written over each other. Positions are now recomputed
  from the current heights whenever the diagram is laid out, and only a card
  someone has dragged by hand stays where they put it.
- **A card stays the same size when it is being edited.** Pressing Edit grew a
  card by two thirds before anything had been edited. The reference field is
  now a popover in the top layer rather than a full-width control inside the
  card, column rows in edit mode get a track for their drop button, and
  dropping a table moved to the card's menu. Toggling Edit now changes a
  card's height by at most a pixel of rounding.
- **Seven undo defects** found by a whole-branch review, two of which lost
  work in ordinary use: `applyState` is now the single place that maintains
  `history.base`, so an undo after an import no longer discards the import;
  and `serialize()` chains `pushPlan`, `undo` and `redo`, closing the race an
  autorepeating `Cmd-Z` opened against a last-write-wins server.
- **The Postgres integration tests never ran.** They opened the database with
  `sql.Open("pgx", …)` but nothing imported `pgx/v5/stdlib`, so no driver by
  that name was registered — and the error was handled with `t.Skipf`, so CI
  set `SEEDORA_TEST_POSTGRES`, every subtest skipped, and the run reported
  success without executing. The driver is imported, and a target named by an
  environment variable that cannot be reached is now `t.Fatalf`.
- A `.pyc` from the browser tests was tracked. Untracked, and `.gitignore` now
  covers Python bytecode and the virtualenv.

### Changed

- The README's `~2,000,000 rows/s` generation claim is replaced with the
  measured figures, the machine they came from, and the command to rerun them.
  It also now documents `--append`, `seedora dump`, `--host`, and the other
  flags the table had been missing.

### Testing

- **The page in a real browser.** Twenty-four Playwright tests over Chromium
  against the 23-table demo schema: no console errors, every table drawn, no
  two cards overlapping at their rendered heights, no edge passing under a
  card, drags surviving a reload, folding, the inspector, zoom-to-fit, a
  seeding run watched over SSE, and no sideways scroll at 1280px. The
  dependency list is deliberately outside the build — nothing here is needed
  to compile, run, or ship Seedora. These found the two seeding bugs above.
- **The diagram's router and layout**, thirty-two headless tests under node's
  own test runner, with no `package.json` and no bundler: `app.js` is loaded
  in a `vm` against a stubbed DOM. All 600 ordered pairs of a 5x5 grid get a
  route crossing no other card.

## [0.3.0] — 2026-08-06

### Added

- **MySQL and MariaDB.** A third engine, end to end: introspection (including
  inline `ENUM` columns, `AUTO_INCREMENT` keys, and `TINYINT(1)` read as
  boolean), the mapping UI, the schema editor, migration history, and seeding
  through chunked multi-row `INSERT`. `mysql://` and `mariadb://` DSNs are
  accepted, as is the native `user:pass@tcp(host:3306)/db` form the MySQL
  client library uses.
- **`--migrations <path>`.** Seedora reads a project's migration directory,
  replays the `CREATE TABLE`, `ALTER TABLE ADD/DROP COLUMN`, and `DROP TABLE`
  statements in filename order, and creates the tables the database is missing
  before seeding them. Down migrations are skipped — a `.down.sql` file, a
  `down/` directory, or the half of a file after `-- +goose Down` or
  `-- migrate:down`. In the UI the missing tables are drawn as drafts and go
  through the usual SQL review dialog; nothing is applied unattended. This is
  not a migration runner: it records nothing, runs nothing in reverse, and
  alters no table that already exists.
- `POST /api/schema/scan` returns what a migrations directory holds and which
  of it the database does not have.
- An example, `examples/mysql-migrations/`: an empty MySQL and a directory of
  `.sql` files, seeded in one command.
- `make mysql-up`, `make mysql-down`, and `make dev-mysql`, and MySQL in CI.
- **Relationships drawn onto a table.** Dragging a key onto another table's
  card — rather than onto one of its columns — proposes the foreign key column
  that table is missing: `users.id` dropped on `orders` becomes
  `orders.user_id REFERENCES users(id)`, named, typed from the parent key, and
  reviewed as SQL before it runs.
- **Cardinality is asked, not assumed.** Dropping a key on a table asks which
  relationship it is before anything is proposed: one-to-many (a foreign key
  column), one-to-one (the same column, unique), or many-to-many (a join table
  named after both sides, with one key from each as its composite primary key).
  Each choice says what it will do to the schema.
- **Join tables seed correctly.** A composite primary key made of foreign keys
  constrains the pair, not either column, and drawing both at random collides
  almost immediately. The pairs are now assigned from the parent pools —
  distinct by construction, spread over the parents, reproducible from the
  seed — and a plan asking for more rows than there are combinations is
  refused before anything is written, with the arithmetic in the message.
- **The cardinality marker is a control.** Clicking the `1:N` label, or either
  crow's foot, asks what that relationship should be and makes it that. It is a
  mapping change first — one-to-one means the seeder gives each parent exactly
  one child — and then offers the unique index that makes the database enforce
  it (`add_unique`, a `CREATE UNIQUE INDEX`, which all three engines accept on a
  table that already exists). The line's context menu carries the same item, for
  the notation that draws no markers.
- **`add_foreign_key`.** After a relationship is drawn between two columns that
  both already exist, Seedora offers to add the database's own constraint, as
  one `ALTER TABLE … ADD FOREIGN KEY`. SQLite refuses it with the reason: there
  is no ALTER for it, and rebuilding the table is not something a seeding tool
  should do behind a drag.

### Changed

- Dragging a primary key onto a plain column now points the plain column at the
  key, not the other way round. A foreign key runs child → parent whichever way
  the mouse went, and the old behaviour made `users.id` a foreign key.

### Fixed

- A unique foreign key — a one-to-one — was filled by the ordinary uniqueness
  repair, which replaces a duplicate with the row's index. On a foreign key that
  is a value pointing at nothing. Those columns are now assigned distinct parent
  keys, and a run wanting more children than there are parents is refused with
  the arithmetic.
- Postgres introspection missed a column held unique by a `CREATE UNIQUE INDEX`
  rather than by a `UNIQUE` constraint: pg_constraint knows nothing about the
  index, and both enforce uniqueness identically. The seeder would generate
  duplicates and fail at the insert. It reads pg_index too now — which matters
  more since that is the spelling Seedora itself writes for a one-to-one.
- The browser's "Failed to fetch" is now the sentence it actually means: the
  server this page came from has stopped, start it again and reload.
- `tests` no longer depends on the order its tests run in: the MySQL smoke test
  built on tables another test created, and dropped none of its own, so CI ran
  it first and failed. It creates its own schema and cleans up after itself.
- staticcheck findings: an unused function in `internal/ui`, and a literal
  `%s` in a config test that hid what the test was checking.
- A name match that could not fit the column's type is no longer applied. `id
  BIGINT PRIMARY KEY` with no auto-increment matched the name "id", which means
  a UUID nearly everywhere else — on a strict engine that failed mid-run, and on
  a lax one it wrote zeros.
- `schema.sql` export now carries the auto-assignment of a key the database
  fills itself, as `AUTO_INCREMENT` on MySQL and `GENERATED BY DEFAULT AS
  IDENTITY` on Postgres. The catalog reports the type without it, so the
  exported script used to recreate the table with a key nobody assigns.
- `ddl.Parse` keeps `AUTO_INCREMENT` and `IDENTITY` as a fact about the column
  rather than dropping the clause, so an imported `.sql` file round-trips.

## [0.2.0] — 2026-08-06

### Added

- **Schema editor.** Tables can be sketched in the diagram and created for
  real: create a table, add a column, drop a column, drop a table. Every batch
  is rendered to SQL and shown before it runs, then applied in one transaction.
- **Draw relationships.** Dragging one column onto another table's column
  points the first at the second — a mapping change, not a migration.
- **Column order.** Columns can be dragged into any order within their table.
  The order is kept in `seedora.yaml` and used for display; a bulk load still
  writes in the order the catalog reports.
- **Import and export in three formats.** The mapping as `seedora.yaml`, the
  schema as `schema.sql`, and the diagram as a Mermaid script. A `.sql` file
  can be imported: its tables arrive as drafts with the SQL to review.
- **Diagram.** Zoom, fold a table down to its header, light up a table's
  relationships, follow one relationship as an animated flow, cardinality
  notation (crow's foot or labels), edges routed around cards rather than
  under them, and a line can be dragged out of the way.
- **Settings.** Relationship notation and flow animation, kept per browser.
- **Schema history.** `GET /api/history` answers "how did this schema get like
  this". No engine records DDL anywhere a query can reach, so it reads whatever
  a migration tool left behind — golang-migrate, Flyway, goose, Alembic, Atlas,
  Django, Knex, Rails — and merges it with the changes Seedora applied itself,
  which are logged per database with the SQL that ran.
- **A demo on GitHub Pages.** The real UI over a recording of the real API,
  generated from the example schema on every push so it cannot drift.
- **`SEEDORA_CONFIG_DIR`** overrides where the per-machine files live, for
  containers and for tests that must not write into a developer's own store.
- **Examples, benchmarks, and integration tests**, plus CI for tests, linting,
  releases, and the demo.

### Fixed

- **Preview's Regenerate produced identical rows.** The preview seed is fixed
  on purpose so a change in the output is a change the user made; Regenerate
  now mixes a nonce into it.
- **Foreign keys previewed as NULL against an empty parent**, which is every
  database nobody has seeded yet. The keys the parent is about to be given are
  generated instead.
- **A composite primary key rendered both an inline `PRIMARY KEY` per column
  and a table-level clause**, which is a syntax error on both engines.
- **`ALTER TABLE … ADD COLUMN` with a foreign key returned two Postgres
  statements joined by a semicolon in one string**, which the extended query
  protocol will not run. Each statement is now separate.
- **An unqualified `name` column was filled with a person's name on every
  table.** It now follows the table: people on `users`, a word on
  `categories`.
- **Page assets could be served from cache in mismatched versions**, producing
  a script that failed on an element the cached page did not have.

## [0.1.0]

First public version.

### Added

- PostgreSQL and SQLite: introspection, bulk insert, truncate.
- Generator inference per column from the catalog and the column's name.
- The mapping UI: a schema diagram where every column carries the generator
  that will fill it.
- `seedora.yaml`, with import and export.
- Seeding with live progress, dependency-ordered inserts, unique constraint
  handling, and a fixed `--seed` for reproducible runs.
- The production-target guard.

[Unreleased]: https://github.com/bakhod1r/seedora/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/bakhod1r/seedora/releases/tag/v0.5.0
[0.4.0]: https://github.com/bakhod1r/seedora/releases/tag/v0.4.0
[0.3.0]: https://github.com/bakhod1r/seedora/releases/tag/v0.3.0
[0.2.0]: https://github.com/bakhod1r/seedora/releases/tag/v0.2.0
[0.1.0]: https://github.com/bakhod1r/seedora/releases/tag/v0.1.0
