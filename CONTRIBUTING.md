# Contributing

Bug reports, engines, and generators are all welcome. This file is what you
need to know before opening a pull request.

## Getting set up

```sh
git clone https://github.com/bakhod1r/seedora
cd seedora
make demo-big   # a throwaway SQLite database with a schema worth looking at
make dev-big    # the UI on http://127.0.0.1:7777
```

`make help` lists the rest. There is no asset pipeline: the UI is one HTML
file, one stylesheet, and one script, embedded with `go:embed`. Editing them
and restarting is the whole loop.

## Before you open a pull request

```sh
make lint   # gofmt, go vet, and the tests under the race detector
```

CI runs the same things. The race detector is not optional: generation runs on
a worker pool while a driver holds an open COPY, and a data race there produces
wrong data rather than a crash.

## What the code expects

**Comments say why, not what.** The code already says what it does. A comment
earns its place by explaining a decision that is not obvious from reading it —
a constraint, a trade-off, a thing that was tried and did not work.

**Errors are for the person who hit them.** `no driver for "mysq" (supported:
postgres, sqlite)` is an error message. `invalid scheme` is not.

**Every problem, not the first one.** Validation returns a slice. A schema that
drifted usually drifted in several places at once, and reporting them one round
trip at a time is the slowest way to find that out.

**Nothing is written without being shown first.** The preview, the SQL dialog,
and the dry run all exist for the same reason: this tool points at a database
someone is working in.

## Adding an engine

`db.Driver` and `db.Tx` are the whole contract — introspect, begin, bulk
insert, truncate, read keys, exec. Two engines have been implemented against
it, so a third is the test of whether the interface is drawn right. If it is
not, say so in the pull request rather than working around it.

A driver registers itself from `init`, so linking it in is what enables it:

```go
func init() { db.Register(open, "mysql") }
```

Introspection has to report enough for inference to work: types, nullability,
defaults, generated columns, uniqueness, primary keys, foreign keys, and the
row count. A driver that guesses one of those makes the whole mapping a guess.

## Adding a generator

Generators come from [Synth](https://github.com/bakhod1r/synth). A new one
belongs there, and Seedora picks it up. What belongs here is the inference: the
column names that should map to it, in `internal/plan/infer.go`.

A name that carries no domain — `name`, `title`, `code` — goes in the
`ambiguous` table instead. It gets applied and marked Low confidence, so the UI
puts it in front of a human rather than silently guessing.

## Tests

Table-driven, against a real database rather than a mock. `internal/db`,
`internal/seed`, and `internal/ui` all open a temporary SQLite file; a fake that
agrees with the code proves nothing about the engine.

A test name should say what is true, not what is exercised:
`TestPreviewFillsForeignKeysFromAnEmptyParent`, not `TestPreview2`.

## Commits

One change per commit, present tense, and a body when the subject cannot carry
the reason. If the change fixes something, say what was broken.

## Reporting a bug

The engine, its version, the DSN's shape with the credentials removed, and the
smallest schema that reproduces it. A `CREATE TABLE` we can paste into a file
is worth more than a description of one.

Security issues do not go in the tracker — see [SECURITY.md](SECURITY.md).
