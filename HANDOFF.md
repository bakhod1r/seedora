# Handoff

Where the work stopped, and what the next session should pick up.

## State

Everything builds, vets, and passes `go test -race ./...` — ten packages.
Nothing is committed yet; `git status` shows the whole tree as untracked.

Working end to end: PostgreSQL, SQLite, and MySQL/MariaDB, introspection, the mapping UI, the
schema editor, `seedora.yaml`, import and export in three formats, schema
history, and per-table seeding with live SSE progress.

Measured on an M4 Pro: generation 1.15M rows/s at 100k rows, 430k at 10k, 220k
at 1k — the rate climbs because a run's fixed cost is paid once. Postgres `COPY`
~244k rows/s, SQLite ~24k. Determinism verified: the same `--seed` produces the
same rows twice, and a different one does not.

`benchmarks/README.md` carries those numbers and what to read out of them. The
README still claims ~2.0M rows/s for generation, which the benchmark does not
reproduce — worth correcting or explaining.

## The schema builder

Tables can be designed in the diagram and created for real. End to end and
covered by tests.

- `internal/ddl` renders `Change` values to SQL for both dialects and validates
  a batch before anything runs: identifier checks, missing types, duplicate
  columns, NOT NULL added to a table that already has rows, SQLite refusing a
  UNIQUE column on ALTER, references to tables being dropped, and reference
  cycles among newly created tables. `Plan` orders creates so a child comes
  after the parent it points at. The package executes nothing.
- `db.Tx.Exec(ctx, stmt)` runs one statement; `db.Driver.Dialect()` reports the
  engine's dialect, and `/api/state` carries it plus `ddl.Types` so the diagram
  knows which types to offer.
- `POST /api/schema/plan` renders SQL and touches nothing. `POST
  /api/schema/apply` runs the same statements in one transaction,
  re-introspects, and merges the fresh inference into the existing plan so
  chosen generators and row counts survive. Apply is refused while a seeding run
  is in flight.
- UI: a draft card for a new table, an Edit mode on an existing one, a change
  count in the top bar, and a dialog showing the generated SQL with Apply and
  Cancel.

Scope is deliberately create table, add column, drop column, drop table.
Changing a column's type is where engines stop agreeing and where getting it
wrong costs data; it is out on purpose, not forgotten.

## Import, export, and history

- **Export** offers three things, because they are three different things:
  the mapping as `seedora.yaml`, the live schema as `schema.sql`
  (`ddl.Script`), and the diagram as a Mermaid script (`ddl.Mermaid`, with
  cardinalities).
- **Import** sniffs the text. YAML is merged as before; a `.sql` file goes
  through `ddl.Parse`, which reads CREATE TABLE and ignores everything else in
  the file rather than failing on it. Tables that already exist are dropped from
  the batch, the rest become drafts, and the SQL dialog opens. `Script` and
  `Parse` round-trip, and a test holds them to it.
- **History** answers "how did this schema get like this". Neither engine
  records DDL anywhere a query can reach, so `db.ReadHistory` reads whatever a
  migration tool left behind — golang-migrate, Flyway, goose, Alembic, Atlas,
  Django, Knex, Rails. The catalogue is closed: a table is read only when its
  name *and* its columns are recognised, so a table that merely shares a name
  cannot break the request.
- Changes applied from the diagram are recorded in `internal/store/applied.go`,
  per database, keyed by the **redacted** DSN. No migration tool will ever
  mention them, so that log is their only trace. `GET /api/history` merges both
  sources, newest first.
- `model.Migration.Applied` is three-state on purpose: true, false, or absent.
  Several tools record no outcome at all, and displaying that as success would
  be a lie about the one thing the dialog exists for.

## The diagram

Most of the recent work is here, and none of it is covered by tests — see the
gaps below.

- **Routing.** Edges are orthogonal with rounded corners; no diagonals. Routes
  are proposed and then *tested* against the card boxes — straight, then down
  the middle, then down each corridor between the cards nearest the middle
  first, then over or under everything. The last always exists. A check on a
  dense 5×5 grid of cards found a clear route for all 600 pairs.
- Every edge leaves the child's right edge and enters the parent's left, the way
  an ER diagram is drawn. It costs length when the parent sits to the left and
  buys arrows that all mean the same thing.
- **Layout.** Tables are grouped into sets that reference each other and drawn
  as separate islands, largest first. Columns come from dependency depth; the
  vertical position is relaxed — each card is pulled towards the average centre
  of what it is joined to, with overlaps separated after each pass. Measured on
  the demo schema: mean vertical distance between related tables fell from 447
  to 242, mean edge length from 819 to 696, no overlaps.
- **Direct manipulation.** Drag a column to reorder it within its table, or onto
  another table's column to point it there (a mapping change, not a migration).
  Drag a line to place a waypoint it must route through. Both are remembered per
  database in `localStorage`.
- Zoom with a rail, the wheel, or the keyboard; fold a table to its header;
  light up a table's relationships; follow one as an animated flow; cardinality
  as crow's foot or labels, switchable in Settings.
- A context menu on the canvas, a card, and a line, replacing the browser's.

Column order is now the user's: `restoreOrder` reconciles it with the live
schema instead of overwriting it, `plan.Merge` no longer replaces it, and
`spec.columnOrder` prefers it. A bulk load still writes in catalog order, which
is what the wire protocol expects.

## Bugs found and fixed

- A composite primary key emitted an inline `PRIMARY KEY` per column *and* a
  table-level clause — a syntax error on both engines.
- `addColumn` returned two Postgres statements joined by a semicolon in one
  string, which the extended query protocol will not run. `Plan` now returns one
  executable statement per element.
- Preview's Regenerate redrew identical rows. The preview seed is fixed on
  purpose so a change in the output is a change the user made; Regenerate now
  mixes a nonce into it.
- Foreign keys previewed as NULL whenever the parent was empty, which is every
  database nobody has seeded yet. The keys the parent is about to be given are
  generated instead.
- An unqualified `name` column was filled with a person's name on every table.
  It now follows the table: people on `users`, a word on `categories`.
- Page assets could be served from cache in mismatched versions, so a new script
  died on an element the cached page did not have. They now revalidate, and the
  script binds through a helper that survives a missing element.
- The tests wrote into the developer's real config directory.
  `SEEDORA_CONFIG_DIR` overrides it, and the UI tests set it to a temp dir.

## Known gaps

- **Nothing covers the page itself.** The diagram is the largest and least
  tested part of the codebase: routing, layout relaxation, the drag gestures,
  and the undo-less state in `app.js` are all verified by looking at them. The
  router and the layout were each checked with a throwaway Node script, which is
  better than nothing and is not a test in the repository.
- `--append`, and the index suggestion the README mentions, do not exist.
- The MySQL integration tests skip unless `SEEDORA_TEST_MYSQL` is set; CI sets
  it, and `make mysql-up` is the one-line way to set it locally.
- The Postgres integration tests in `tests/` skip unless
  `SEEDORA_TEST_POSTGRES` is set. CI sets it; a laptop usually does not, so they
  are effectively unexercised locally.

## Asked for and deliberately not built

- **Showing rows already in the database in the preview.** It needs a
  `db.Tx.Sample`, and it turns a page that exposes a schema into one that
  exposes data — with no authentication, reachable by any site in the user's
  browser. The guard for it (Sec-Fetch-Site, Origin, loopback-only Host) was
  written and then removed at the user's request. If this comes back, that guard
  comes back first, along with a row cap, redaction of password-shaped columns,
  and a refusal to sample when bound to a non-loopback address.
- **A DDL audit trigger.** Postgres event triggers would give real ALTER
  history, and installing one into somebody else's database is not something a
  seeding tool should do.

## MySQL (v0.3)

The third engine, and the answer to whether `db.Driver` was drawn right or
fitted to two: the interface needed no change. What MySQL needed was three
concessions in the driver, each documented at its site.

- **Quoting.** The codebase writes identifiers with double quotes, so the
  connection sets `sql_mode = ANSI_QUOTES` (plus `STRICT_ALL_TABLES`, so an
  over-long value is an error rather than a silent truncation). DDL rendered by
  `internal/ddl` uses backticks instead, which mean the same thing in either
  mode, so an exported `schema.sql` runs on a session Seedora did not set up.
- **DDL commits.** MySQL commits the open transaction before every DDL
  statement, so a schema edit cannot be rolled back. The statements are still
  validated, rendered, and ordered, and the review dialog says what MySQL will
  do rather than repeating the promise the other engines make.
- **Truncate is `DELETE`.** `TRUNCATE` is DDL there and would commit the
  transaction a failed run has to unwind. The cost is that emptying a table
  another table still points at is refused; the error names the constraint and
  says to include that table in the run. Postgres hides the same case behind
  `TRUNCATE … CASCADE`, which deletes rows from tables nobody named.

Enums are inline on MySQL rather than named types, so one is synthesised per
column as `table_column`. `TINYINT(1)` is read as boolean, since that spelling
is the only signal a 0/1 column means true/false.

## Migrations (v0.3)

`ddl.Scan` reads a directory of `.sql` files, replays them, and `ddl.Missing`
splits the result against the live schema. `--migrations` wires it to `run`
(create the missing tables, then seed) and to the UI (draw them as drafts,
review the SQL, apply on a click). `POST /api/schema/scan` is the endpoint.

It is deliberately not a migration runner. It writes nothing to a migration
tool's table, runs nothing in reverse, and alters no table that already exists —
adding any of that would make it a second, worse implementation of a tool every
project already has.

## Two upstream findings

- **synth** had a data race in `locale.Get` — a lazy write to a shared
  `*Locale`. Reported, fixed upstream in `v1.4.6`, which this repo now uses.
- **oneenv** is sound. The bug was mine: `WithExpand()` on a config struct whose
  main field is a DSN. Expansion reads `$` as a variable, so a password of
  `$ecret123` expanded to nothing. Expansion is off, and
  `internal/config/config_test.go` keeps it off.
