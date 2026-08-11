# Supporting 25 database engines

Seedora ships as one Go binary with no cgo, no runtime, and nothing to install
alongside it. That promise, not driver availability, is what decides how the
next twenty-two engines get added — and it decides it early enough to be worth
settling before any of them is written.

**Every engine on this list has a working pure-Go driver except DuckDB.** That
was measured rather than assumed. The constraint is elsewhere: linking them all
into one executable turns a 16 MB binary into a 100 MB-class one, and three
cloud warehouse SDKs account for most of it.

| | |
| --- | --- |
| Seedora today | 15.9 MB |
| Engines needing no cgo | 24 of 25 |
| Added if all were linked | ~109 MB |
| Blocked engines | 1 (DuckDB) |

## How these numbers were produced

Two questions decide whether a driver is usable here, and both have exact
answers. Does it build with cgo disabled — because the release workflow
cross-compiles five targets, and turning cgo on means a C toolchain for each.
And what does it cost in bytes.

Grepping for `import "C"` is **not** the test: several drivers carry cgo files
that build tags exclude. Snowflake, MongoDB, and SAP HANA all look like cgo by
that measure and all build cleanly without it. The test is the build.

```sh
# does it link without cgo?
CGO_ENABLED=0 go build -o /dev/null .

# what does it cost? a program that imports the driver and does nothing,
# built the way the release workflow builds Seedora
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o probe .
```

Every figure below is the second command's output minus a 1.0 MB empty-program
baseline, on darwin/arm64, Go 1.26, against the latest release of each driver.

## What each driver costs

The spread is two orders of magnitude. A wire-protocol driver is one to three
megabytes; a cloud SDK is fifteen to twenty-six, because it arrives with gRPC,
protobuf, and a generated API surface for the whole platform.

| Driver | Net MB |
| --- | ---: |
| Neo4j | 1.3 |
| Redis | 1.5 |
| SQL Server | 1.6 |
| Cassandra | 1.6 |
| Vertica | 1.6 |
| SAP HANA | 1.9 |
| Trino | 2.2 |
| DynamoDB | 2.3 |
| InfluxDB | 2.3 |
| Elasticsearch | 2.4 |
| MongoDB | 3.6 |
| ClickHouse | 4.6 |
| Databricks | 5.8 |
| BigQuery | 15.4 |
| Oracle | 16.2 |
| Snowflake | 18.2 |
| Cloud Spanner | 26.1 |

The sum is 109 MB. The real total would be lower, since the Google and
Databricks SDKs share large dependency trees — but the order of magnitude is the
point, and it does not change: one binary cannot hold all of them and stay a
thing you download and run.

## The decision this forces

Build tags, and the existing design already accommodates them. Drivers register
themselves against DSN schemes in `init()`, and the only thing that links one in
is a blank import in `cmd/seedora`. Nothing above the driver layer branches on
the engine. So the change is a set of build constraints and a release matrix —
not a refactor.

- **Default build** — the engines a developer seeds against on their own
  machine: PostgreSQL, MySQL, SQLite, SQL Server, ClickHouse. Under 25 MB, still
  one download.
- **`-tags warehouse`** — Snowflake, BigQuery, Redshift, Databricks, Spanner.
  The heavy SDKs, wanted by the people who want them and by nobody else.
- **`-tags nosql`** — MongoDB, Redis, Cassandra, Elasticsearch, Neo4j, DynamoDB.
  Cheap in bytes, expensive in concepts; see the schema problem below.
- **`-tags all`** — everything, for the container image where size is somebody
  else's problem.

The alternative — a plugin system — is worse here. Go plugins do not
cross-compile, do not work on Windows, and would reintroduce exactly the
runtime-to-install problem the single binary exists to avoid.

## The engines, and what each actually needs

Grouped by the work involved rather than by popularity, because the work is what
differs.

### Already working — 3

| Engine | Bulk path |
| --- | --- |
| PostgreSQL | `COPY` |
| MySQL / MariaDB | chunked multi-row `INSERT` |
| SQLite | prepared chunked `INSERT` |

### Wire-compatible — 4, and nearly free

These speak PostgreSQL or MySQL on the wire. The driver already accepts their
DSNs; what is missing is that nobody has run the test suite against one, which
the README says in as many words. The work is a container in CI and a row in the
table — no new driver code at all. This is the cheapest honesty available.

| Engine | Speaks | Work |
| --- | --- | --- |
| CockroachDB | postgres | CI container, verify `COPY` and catalog |
| YugabyteDB | postgres | CI container, verify `COPY` and catalog |
| Aurora (PG / MySQL) | postgres, mysql | Cannot be containerised; document as untested |
| TiDB | mysql | CI container, verify the MySQL concessions hold |

### New relational drivers — 5

The straightforward group: a catalog to introspect, a bulk-insert path,
transactions that behave. Each is one package under `internal/db` implementing
the existing two interfaces.

| Engine | Driver | Net MB | Bulk path |
| --- | --- | ---: | --- |
| SQL Server | `microsoft/go-mssqldb` | 1.6 | TDS bulk copy |
| Oracle | `sijms/go-ora` | 16.2 | array binding |
| SAP HANA | `SAP/go-hdb` | 1.9 | batch `INSERT` |
| Vertica | `vertica/vertica-sql-go` | 1.6 | `COPY LOCAL` |
| Firebird | `nakagami/firebirdsql` | — | batch `INSERT` |

### Analytical — 7, and the transaction promise breaks

Seedora's central guarantee is that a run is one transaction and a failure
leaves the database exactly as it was. Most warehouses cannot honour that:
BigQuery and Snowflake commit as they load, and a run that dies halfway leaves
half a table. This is the same class of problem as MySQL's non-rollbackable DDL,
which is handled by saying so at the point of use rather than pretending
otherwise — and the same answer applies.

| Engine | Net MB | Bulk path | Rollback |
| --- | ---: | --- | --- |
| ClickHouse | 4.6 | native batch | no |
| Snowflake | 18.2 | `PUT` + `COPY INTO` | no |
| BigQuery | 15.4 | storage write API | no |
| Redshift | ~0 | `COPY` from S3, or postgres wire | yes |
| Databricks | 5.8 | staged `COPY INTO` | no |
| Trino | 2.2 | `INSERT`, connector-dependent | varies |
| DuckDB | — | appender API | **blocked: cgo** |

DuckDB is the one engine here that cannot be added without giving up the binary.
Its Go driver bundles a C++ library across 25 cgo files; linking it means
`CGO_ENABLED=1` and a cross-compiler for each of five release targets. Worth
revisiting only if a pure-Go implementation appears — and worth saying no to
clearly until then, rather than leaving it on a roadmap as though it were
coming.

### Non-relational — 6, and a different product question

The drivers are cheap and every one is pure Go. The problem is above them:
Seedora infers what data should look like from a catalog of tables and columns,
and these engines do not have one. There is nothing to introspect, so there is
nothing to propose, so the mapping UI has no rows to show.

That is not a driver — it is a second way of learning a schema, and it needs its
own decision before any of these is written. Three candidate sources: sample the
documents already stored and infer from them, read a JSON Schema the project
already keeps, or let the user write a document template. All three are real
features. None is a `db.Driver`.

| Engine | Net MB | What a "table" is | Schema from |
| --- | ---: | --- | --- |
| MongoDB | 3.6 | collection | sampling, JSON Schema |
| Elasticsearch | 2.4 | index | mapping API — the one that has a real schema |
| Cassandra / Scylla | 1.6 | table | `system_schema` — genuinely relational |
| DynamoDB | 2.3 | table | key schema only; the rest is sampling |
| Neo4j | 1.3 | node label, relationship type | `db.schema` procedures |
| Redis | 1.5 | key pattern | nothing — a template is the only option |

Cassandra and Elasticsearch are the two that fit the existing model almost
as-is: both have a real, queryable schema with typed columns. They belong before
the others for that reason, not because they are more popular.

## What has to change above the driver layer

- **Build tags and a release matrix.** Five targets times four tag sets is
  twenty artefacts, or fewer if the warehouse and nosql builds ship only for
  linux and darwin. Decide before the first heavy driver lands, not after.
- **A transaction contract that admits the truth.** `db.Tx` promises rollback.
  Warehouses cannot deliver it. The driver should declare what it can honour,
  and the UI should say so before a run rather than after a failure — the
  pattern the MySQL DDL concession already set.
- **A schema source that is not a catalog.** Required by all six non-relational
  engines and by nothing else. Worth designing once, deliberately, rather than
  six times inside six drivers.
- **Staged bulk loading.** Snowflake, Redshift, BigQuery, and Databricks all
  load fastest from object storage, not over a connection. That is a genuinely
  different `Insert` — write files, then tell the warehouse to read them — and
  `seedora dump` is already most of it.
- **Test infrastructure per engine.** Containers exist for most. The cloud
  warehouses need accounts and cost money per run, so they cannot be CI-tested;
  they need a manual verification checklist and honest labelling instead.

## Order of work

Sequenced so each phase pays for itself and the expensive architectural
decisions happen before the code that depends on them.

1. **Verify what already works.** CockroachDB, YugabyteDB, and TiDB in CI. No
   new driver code. Turns four claims in the README from "designed" into
   "tested", which is the highest ratio of credibility to effort available
   anywhere on this list.
2. **SQL Server.** The most-asked-for engine, 1.6 MB, pure Go, a real bulk copy
   path, and a container to test against. It also proves the driver interface
   survives a third dialect family — the same thing MySQL proved about the
   second.
3. **Build tags, before anything heavy.** The mechanism, the release matrix, and
   the documentation of which build has which engines. Cheap now; a migration
   later, once four warehouse drivers are already linked in.
4. **ClickHouse, then the transaction contract.** The first engine that cannot
   roll back. Small enough to be a fair test case and popular enough to be worth
   the design work it forces.
5. **Cassandra and Elasticsearch.** The two non-relational engines with a real
   schema to read. They extend the reach without needing the sampling work
   first, and they establish how far the existing model actually stretches.
6. **The schemaless question.** Design sampling, JSON Schema, and templates as
   one feature. Then MongoDB, then the rest — each becomes a small driver once
   this exists, and an argument if it does not.
7. **Warehouses, on staged loading.** Snowflake, BigQuery, Redshift, Databricks.
   Last because they are the heaviest, the least testable, and the most
   dependent on everything above.

## What this plan says no to

- **DuckDB**, until a pure-Go driver exists. Everything else on the list is
  reachable without giving up the binary; this one is not, and it should not sit
  on a roadmap pretending otherwise.
- **One binary with everything in it.** Measured, not assumed: it would be a
  100 MB-class download to give every user twenty-two engines they do not have.
- **Go plugins.** No cross-compilation, no Windows, and a runtime to install —
  which is the problem the single binary was the answer to.
- **Claiming an engine works before a test says so.** Four of these already
  speak a wire protocol Seedora supports, and the README is careful to call them
  untested. That care is worth keeping as the list grows.
