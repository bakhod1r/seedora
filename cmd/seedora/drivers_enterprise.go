//go:build enterprise || all

// The engines bought rather than downloaded. Four of them are cheap in bytes —
// Vertica 1.6 MB, SAP HANA 1.9 MB, Firebird and Trino about 2 MB each — and
// would fit in the default build on size alone. They are behind a tag because
// Oracle is not: at 16.2 MB it is the single heaviest driver outside the cloud
// warehouses, and splitting the group to keep it out would leave a tag holding
// one engine.
//
// docs/engines.md assigns the other three tag sets and leaves these five
// unplaced; this file is where that decision was made.

package main

import (
	_ "github.com/bakhod1r/seedora/internal/db/firebird"
	_ "github.com/bakhod1r/seedora/internal/db/hana"
	_ "github.com/bakhod1r/seedora/internal/db/oracle"
	_ "github.com/bakhod1r/seedora/internal/db/trino"
	_ "github.com/bakhod1r/seedora/internal/db/vertica"
)
