# Benchmarks

What Seedora is fast at, measured rather than claimed.

```sh
go test ./benchmarks/ -bench . -benchtime 3s
```

Generation is measured on its own, because it is the part that has to stay
ahead of the database: on Postgres the whole table is one `COPY`, and if
generation is slower than the socket drains then the server waits on Go instead
of the other way round.

The numbers below are from an Apple M4 Pro, Go 1.26, and are a shape rather
than a promise — another machine will differ, but the ratios will not.

| What | Rate |
|---|---|
| Generation, ten mixed columns, 100k rows | ~1.15M rows/s |
| Generation, 10k rows | ~430k rows/s |
| Generation, 1k rows | ~220k rows/s |
| PostgreSQL insert via `COPY` | ~244k rows/s |
| SQLite insert, multi-row prepared statement | ~24k rows/s |

Two things to read out of that.

The rate climbs with the row count, because the fixed cost of a run — compiling
the spec, opening the transaction, reading parent keys — is paid once. Below a
few thousand rows it is most of the wall clock, and no amount of generator
tuning will show up.

Generation at scale is four times faster than `COPY` and fifty times faster
than SQLite. That is the design working: the database is the bottleneck and the
generator is never what a `COPY` is waiting for. It also means tuning the
generators buys nothing on SQLite and very little anywhere else.

## Writing one

Benchmarks that touch a database belong here rather than in `internal`, so
`go test ./...` stays fast and hermetic. Anything measuring pure generation can
live next to the code it measures.
