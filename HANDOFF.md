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
top-level README claimed ~2.0M rows/s, which the benchmark does not reproduce;
it now carries the measured figures, names the machine, and says how to rerun
them.

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

Most of the recent work is here. The geometry is covered by `tests/ui/`; the
rendering is not — see the gaps below.

- **Routing.** Edges are orthogonal with rounded corners; no diagonals. Routes
  are proposed and then *tested* against the card boxes — straight, then down
  the middle, then down each corridor between the cards nearest the middle
  first, then over or under everything. The last always exists. All 600 pairs on
  a dense 5×5 grid get a clear route, and `tests/ui/router.test.mjs` is what
  says so on every run. `routePoints` is the decision and `routeAround` wraps it
  in a rounded path, split apart so the test can ask whether a route crosses a
  card — the rounded corners no longer sit on the corners, so the path string
  cannot be checked.
- Every edge leaves the child's right edge and enters the parent's left, the way
  an ER diagram is drawn. It costs length when the parent sits to the left and
  buys arrows that all mean the same thing.
- **Layout.** Tables are grouped into sets that reference each other and drawn
  as separate islands, largest first. Columns come from dependency depth; the
  vertical position is relaxed — each card is pulled towards the average centre
  of what it is joined to, with overlaps separated after each pass. Measured on
  the demo schema: mean vertical distance between related tables fell from 447
  to 242, mean edge length from 819 to 696, no overlaps.
- **Direct manipulation.** Drag a column to reorder it within its table, onto
  another table's column to point it there (a mapping change), or onto another
  table's card to add the foreign key column that table does not have yet (a
  schema change, reviewed as SQL). Direction is the schema's, not the mouse's:
  a primary key dropped on a plain column makes the plain column the child.
  Dropping on a card asks the cardinality first — one-to-many, one-to-one, or
  many-to-many — because those are three different schemas and guessing wrong
  is a migration somebody has to write later. Many-to-many produces the join
  table; `internal/seed/composite.go` is what makes one seedable, by assigning
  key pairs from the parent pools rather than drawing them. The same file covers
  a one-to-one — a single unique foreign key — for the same reason: the ordinary
  uniqueness repair replaces a duplicate with the row index, which on a foreign
  key is a value pointing at nothing.
- **The cardinality marker is a control.** Clicking `1:N` or a crow's foot opens
  the same question and applies the answer: `plan.unique` on the child column,
  then an optional `add_unique` (`CREATE UNIQUE INDEX`) so the database enforces
  it. Loosening a constraint the database already has is deliberately not
  offered — it means dropping an index this tool did not create and cannot name.
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

- **Undo** covers plan edits, fed from `pushPlan` — the one place that knows an
  edit happened and the server took it. The whole plan is snapshotted rather
  than a diff, bounded at 25; an edit the server refused records nothing, and a
  failed undo restores the stack. Schema changes are deliberately not covered:
  they are applied in a transaction after a SQL review, and undoing an applied
  `DROP TABLE` would mean inventing a table this tool never saw.

## Undo shipped with seven known defects

A whole-branch review of the undo work recommended against merging it. It was
merged anyway, deliberately, so these are open and this is the list. Two of
them lose a user's work in ordinary use.

- **`history.base` goes stale whenever the plan changes outside `pushPlan`,
  `undo`, or `redo`.** `applyState` replaces `app.plan` and does not touch
  `base`. Import, schema apply, the post-run state refresh, and `importSchema`
  all take that path, and none of them changes `s.target`, so the reconnect
  check does not fire either. Edit, then import a `seedora.yaml`, then change
  one generator: a single undo throws the imported mapping away, and Save
  writes that to disk. The fix is to make `applyState` the one place that
  maintains `base`, and to `resetHistory()` on a successful import or schema
  apply — those snapshots are no longer edits of the current plan.
- **Nothing serialises in-flight plan requests.** Holding Cmd-Z autorepeats, so
  a second `undo` starts while the first is still in flight: `future` gets two
  entries holding the same plan, a redo step is lost, and two PUTs race a
  last-write-wins server. Two fast edits have the same shape. A flag or a
  promise chain over the three functions closes it.
- **The commonest undo label is the string "undefined".** `commit()` builds it
  from `cur.column`, and `current()` returns `{t, c, cp}` — no such property.
  Every generator change toasts "Undid undefined settings". It should be
  `cur.c.name`.
- **The failure path aliases rather than clones.** `undo` and `redo` both do
  `app.plan = current` in their catch blocks, where `current` is the `base`
  object itself, so later in-place edits mutate the snapshot and the next undo
  becomes a no-op. `structuredClone`.
- **Undo is live during a seeding run.** `setRunningUI` disables the controls
  that would disturb a run and was not told about the two new buttons. The run
  is safe — the Go side captured the plan before starting — but `renderDiagram`
  destroys every progress bar mid-stream.
- **A schema apply leaves the history intact.** Undoing across an applied
  `DROP TABLE` PUTs a plan naming a table that no longer exists, which
  `plan.Validate` refuses. Same fix as the first item.
- **The keystroke comment says the opposite of the code.** It claims the
  shortcut works while a field has focus; the guard is `!inField`, so it works
  in no field at all. The behaviour is the intended one — the browser's own
  undo is what a person typing means — and the comment is simply wrong.

None of these is caught by a test, which is its own finding: both suites are
strictly awaited, so no test can see the concurrency, and every test supplies
its own label, so none sees the "undefined".

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

- **The page is covered at two levels now.** `tests/ui/` loads `app.js`
  headlessly and tests the router and the layout relaxation against boxes it
  supplies. `tests/browser/` drives a real Chromium: the layout as the browser
  computes it, edges checked against what was actually rendered, dragging,
  folding, zoom, and a seeding run driven from the UI. Still uncovered: the
  schema editor's dialogs and the context menus.
- The index suggestion the README mentions does not exist, and the README says
  so.
- The integration tests in `tests/` cover whichever engines the environment
  names. `make pg-up` and `make mysql-up` start both, `make test-all` runs
  everything against them. A DSN that is set but does not work is now a failure
  rather than a skip — see below.

## Two bugs the browser tests found

Both were reproducible from the command line once seen, and both broke
`seedora run` against this repository's own large demo schema — 23 tables,
which is the first schema either shape appears in.

- **A self-reference poisoned the foreign-key pool.** `categories.parent_id`
  points at `categories.id`, and it is resolved before a single row of
  `categories` has been written, so the read is legitimately empty. That empty
  pool was then cached under `categories.id`, and every later child of
  `categories` got a cache hit on it — a NULL in `products.category_id`, which
  is NOT NULL, so the run died two tables later naming a column whose plan was
  correct. An empty pool is no longer cached: emptiness is the one thing about
  a parent that changes during a run.

- **A primary key that is also a foreign key was not seen as a one-to-one.**
  `keyColumns` asked `cp.Unique || c.Unique`, and `Column.Unique` is read from
  the unique indexes — a single-column primary key does not always have one,
  and on SQLite an `INTEGER PRIMARY KEY` is the rowid, so `pragma_index_list`
  reports nothing. The one-to-one assigner declined the table, the ordinary
  uniqueness repair replaced the duplicate with the row index, and that is a
  foreign key pointing at nothing. Being the sole primary key now counts as
  unique, asked directly rather than through the index list.

## The Postgres tests had never run

`tests/seed_test.go` reads the database back through `database/sql` and opened
it with `sql.Open("pgx", …)`, but nothing imported `pgx/v5/stdlib`, so no driver
by that name was registered. The error was handled with `t.Skipf`, which meant
the suite reported success without executing: CI set `SEEDORA_TEST_POSTGRES`,
every Postgres subtest skipped, and the run was green.

Both halves are fixed. The stdlib driver is imported — it is part of `pgx/v5`,
so no new dependency — and a target named by an environment variable that cannot
be reached is now `t.Fatalf`. Setting the variable is a request to run those
tests, and a skip is the wrong answer to a request.

## The UI answers only to its own page

`internal/ui/origin.go` wraps every route. Two checks, because there are two
questions: `Sec-Fetch-Site` and `Origin` say whether the request came from the
Seedora page, and the `Host` header says whether it was addressed to Seedora
rather than to a name an attacker resolved to `127.0.0.1`. Only the second
catches DNS rebinding, and only the first catches an ordinary cross-site POST.

Reads are covered as well as writes: `/api/state` is the schema and
`/api/connections` is every DSN this machine has connected to. Requests with
neither header are allowed — curl, CI, the tests — because no page can produce
one. Binding past loopback with `--host` relaxes the `Host` check alone.

## --append

Adds rows to tables that already hold some. Nothing is truncated, including
tables the plan asks to truncate, since that setting describes a run starting
from empty. Every unique column is read back before generation so the new rows
are unique against the old, and an integer collision counts on from the largest
existing value rather than from the row index, which restarts at zero each run.

`key()` now widens every integer to `int64`. This is what makes the preload work
across drivers: SQLite returns `int64`, pgx returns `int32` for an `integer`
column, and as map keys those are different values — the set would not have
recognised the id it was about to duplicate. Covered against all three engines
in `tests/append_test.go`.

Refused: `--append` with `--truncate`, and `--append` on a join table, whose
uniqueness is over the pair of foreign keys and whose existing pairs cannot be
read back one column at a time.

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
