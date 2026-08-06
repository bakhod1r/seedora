# Fixtures in CI

The case for committing `seedora.yaml`: every developer and every CI run gets
the same database, and the mapping is reviewed like any other file.

```yaml
# .github/workflows/test.yml
      - name: Seed the test database
        run: |
          go run github.com/bakhod1r/seedora/cmd/seedora@latest run \
            --dsn "$DATABASE_URL" \
            --config ./seedora.yaml \
            --truncate \
            --seed 42
        env:
          DATABASE_URL: postgres://postgres:postgres@localhost:5432/app_test?sslmode=disable
```

## Why each flag is there

- `run` skips the UI. It reads the config, seeds, and exits with a status.
- `--truncate` empties the target tables first, so a re-run is a re-seed rather
  than a second helping.
- `--seed 42` fixes the generator. The same commit produces the same rows, so a
  test that asserts on row 7 keeps passing for the right reason. Leave it out
  and every run is different — which is a fine way to find tests that depend on
  data they should not.
- No `--i-know-what-im-doing`. If this ever points at something that looks like
  production, the run should fail.

## Validating instead of seeding

A config drifts when the schema changes. This fails the build when it has,
without writing anything:

```sh
seedora validate --dsn "$DATABASE_URL" --config ./seedora.yaml
```

Worth running in the same workflow that runs the migrations.
