# Integration tests

End-to-end runs against a real database: introspect, infer, seed, and then
check the rows that landed. The unit tests under `internal/` prove the pieces;
these prove the thing.

```sh
go test ./tests/
```

SQLite needs nothing. Postgres runs only when it is pointed at a database it is
allowed to destroy:

```sh
SEEDORA_TEST_POSTGRES='postgres://localhost:5432/seedora_test?sslmode=disable' go test ./tests/
```

Without that variable the Postgres tests skip rather than fail, so a clone with
no server still goes green.

**These tests drop and recreate tables.** Point them at a throwaway database.
