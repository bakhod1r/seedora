# Changelog

Notable changes, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org/spec/v2.0.0.html).

Until `1.0.0`, a minor version may change `seedora.yaml` in a way that needs an
edit. Anything that does is called out under **Changed** with what to do about
it.

## [Unreleased]

### Fixed

- **Folded cards no longer overlap.** A card's position was computed once and
  then kept, so it was still the position worked out for the height the card
  had at the time. Folding or unfolding changes that height, and the diagram
  ended up with cards written over each other. Positions are now recomputed
  from the current heights whenever the diagram is laid out, and only a card
  someone has dragged by hand stays where they put it.

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

[Unreleased]: https://github.com/bakhod1r/seedora/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/bakhod1r/seedora/releases/tag/v0.2.0
[0.1.0]: https://github.com/bakhod1r/seedora/releases/tag/v0.1.0
