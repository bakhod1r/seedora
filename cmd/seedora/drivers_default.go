// The engines a developer seeds against on their own machine. PostgreSQL,
// MySQL and SQLite are imported by main.go itself, since a build with no
// engines at all would be a binary that cannot do anything; these two are the
// rest of the default set.
//
// SQL Server earns its place at 1.6 MB, and ClickHouse at 4.6 MB. Everything
// heavier is behind a tag: see drivers_enterprise.go, drivers_warehouse.go and
// drivers_nosql.go, and docs/engines.md for why one binary cannot hold them
// all.

package main

import (
	_ "github.com/bakhod1r/seedora/internal/db/clickhouse"
	_ "github.com/bakhod1r/seedora/internal/db/sqlserver"
)
