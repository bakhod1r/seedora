# Examples

Each of these is a whole workflow, not a fragment. Run them against a
throwaway database.

| Example | What it shows |
|---|---|
| [`sqlite-quickstart/`](sqlite-quickstart) | Empty file to seeded database in three commands, no config |
| [`ecommerce/`](ecommerce) | A twenty-three table schema with all four cardinalities, and a `seedora.yaml` for it |
| [`mysql-migrations/`](mysql-migrations) | An empty MySQL and a directory of `.sql` migrations, seeded in one command |
| [`ci-fixtures/`](ci-fixtures) | A committed mapping, seeded in a workflow with a fixed seed |

The schema behind the `ecommerce` example is also what `make demo-big` builds,
so it can be opened in the UI with one command.
