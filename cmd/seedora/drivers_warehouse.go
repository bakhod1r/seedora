//go:build warehouse || all

// The cloud warehouses: Snowflake 18.2 MB, BigQuery 15.4 MB, Databricks
// 5.8 MB. They arrive with gRPC, protobuf and a generated API surface for the
// whole platform, which is most of the reason a build with everything linked is
// a 100 MB-class download.
//
// Redshift costs nothing extra — it speaks the Postgres wire protocol and
// reuses pgx, which the default build already carries — but it belongs with the
// engines it is used alongside rather than with the one it borrows a driver
// from.
//
// None of these four can honour Seedora's promise that a run is one transaction
// a failure undoes; Redshift is the exception that can. Each driver says so at
// the point of use, and its Rollback reports what is already permanent rather
// than returning nil as though it had undone something.

package main

import (
	_ "github.com/bakhod1r/seedora/internal/db/bigquery"
	_ "github.com/bakhod1r/seedora/internal/db/databricks"
	_ "github.com/bakhod1r/seedora/internal/db/redshift"
	_ "github.com/bakhod1r/seedora/internal/db/snowflake"
)
