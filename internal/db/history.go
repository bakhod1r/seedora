package db

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bakhod1r/seedora/internal/model"
)

// A database keeps no record of the DDL that shaped it. Neither Postgres nor
// SQLite stores past ALTERs anywhere a query can reach: the catalog describes
// what is, not how it got that way.
//
// What usually does exist is a migration tool's own bookkeeping table. Every
// tool worth using writes one, and reading it is the only way to answer "how
// did this schema get like this" without installing an event trigger into
// somebody else's database or parsing their server logs.
//
// This file is the catalogue of those tables. Each entry is a table name, the
// columns to read, and how to turn a row into a Migration. Nothing here is
// engine-specific, so a driver supplies two functions — one to say whether a
// table exists, one to run a SELECT — and gets the whole catalogue.

// Reader runs a read-only query and returns its rows as field→value maps.
type Reader func(ctx context.Context, query string) ([]map[string]any, error)

// migrationSource is one tool's bookkeeping table.
type migrationSource struct {
	// source names the tool, and is what the UI shows.
	source string
	// table is the bookkeeping table, unqualified.
	table string
	// columns are read in this order. A tool that has none of them beyond the
	// version still works: the rest are optional.
	columns []string
	// order is the ORDER BY, chosen so the newest entry is last.
	order string
	// row turns one row into a Migration.
	row func(map[string]any) model.Migration
}

// migrationSources are the tools common enough to be worth knowing about. The
// list is deliberately closed: a table is only read when its name and its
// columns are both known, so a table that merely shares a name with one of
// these cannot make the query fail.
var migrationSources = []migrationSource{
	{
		// golang-migrate, and Rails before it. One row, holding the version the
		// database is currently at — a position rather than a history, which is
		// what `dirty` makes it worth showing anyway.
		source:  "golang-migrate",
		table:   "schema_migrations",
		columns: []string{"version", "dirty"},
		order:   "version",
		row: func(r map[string]any) model.Migration {
			applied := !asBool(r["dirty"])
			return model.Migration{
				Source:  "golang-migrate",
				Version: asString(r["version"]),
				Name:    ifTrue(asBool(r["dirty"]), "left dirty by a failed migration", ""),
				Applied: &applied,
			}
		},
	},
	{
		source:  "flyway",
		table:   "flyway_schema_history",
		columns: []string{"version", "description", "installed_on", "success"},
		order:   "installed_rank",
		row: func(r map[string]any) model.Migration {
			applied := asBool(r["success"])
			return model.Migration{
				Source:    "flyway",
				Version:   asString(r["version"]),
				Name:      asString(r["description"]),
				AppliedAt: asTime(r["installed_on"]),
				Applied:   &applied,
			}
		},
	},
	{
		source:  "goose",
		table:   "goose_db_version",
		columns: []string{"version_id", "is_applied", "tstamp"},
		order:   "id",
		row: func(r map[string]any) model.Migration {
			applied := asBool(r["is_applied"])
			return model.Migration{
				Source:    "goose",
				Version:   asString(r["version_id"]),
				AppliedAt: asTime(r["tstamp"]),
				Applied:   &applied,
			}
		},
	},
	{
		// Alembic stores the current revision and nothing else — no history, no
		// timestamp. Showing it is still worth it: it says which revision the
		// schema claims to be at.
		source:  "alembic",
		table:   "alembic_version",
		columns: []string{"version_num"},
		order:   "version_num",
		row: func(r map[string]any) model.Migration {
			return model.Migration{
				Source:  "alembic",
				Version: asString(r["version_num"]),
				Name:    "current revision",
			}
		},
	},
	{
		source:  "atlas",
		table:   "atlas_schema_revisions",
		columns: []string{"version", "description", "executed_at"},
		order:   "version",
		row: func(r map[string]any) model.Migration {
			return model.Migration{
				Source:    "atlas",
				Version:   asString(r["version"]),
				Name:      asString(r["description"]),
				AppliedAt: asTime(r["executed_at"]),
			}
		},
	},
	{
		source:  "django",
		table:   "django_migrations",
		columns: []string{"app", "name", "applied"},
		order:   "id",
		row: func(r map[string]any) model.Migration {
			applied := true
			return model.Migration{
				Source:    "django",
				Version:   asString(r["name"]),
				Name:      asString(r["app"]),
				AppliedAt: asTime(r["applied"]),
				Applied:   &applied,
			}
		},
	},
	{
		source:  "knex",
		table:   "knex_migrations",
		columns: []string{"name", "batch", "migration_time"},
		order:   "id",
		row: func(r map[string]any) model.Migration {
			applied := true
			return model.Migration{
				Source:    "knex",
				Version:   asString(r["name"]),
				Name:      ifTrue(r["batch"] != nil, "batch "+asString(r["batch"]), ""),
				AppliedAt: asTime(r["migration_time"]),
				Applied:   &applied,
			}
		},
	},
	{
		// Rails, whose table is the same name as golang-migrate's but holds one
		// row per migration rather than one row in total. Which of the two is
		// present is decided by the column list, so both can be tried.
		source:  "rails",
		table:   "ar_internal_metadata",
		columns: []string{"key", "value", "updated_at"},
		order:   "key",
		row: func(r map[string]any) model.Migration {
			return model.Migration{
				Source:    "rails",
				Version:   asString(r["value"]),
				Name:      asString(r["key"]),
				AppliedAt: asTime(r["updated_at"]),
			}
		},
	},
}

// historyLimit bounds what is read from any one table. A project with a
// thousand migrations has a thousand rows here, and the last hundred are the
// ones anybody looks at.
const historyLimit = 200

// ReadHistory walks the catalogue and returns everything found, newest last.
//
// A table that is absent is skipped, and a table that is present but fails to
// read is skipped too: this is a display feature, and a schema whose
// bookkeeping table has been altered by hand should not stop the page loading.
func ReadHistory(
	ctx context.Context,
	quote func(string) string,
	hasTable func(string) bool,
	read Reader,
) []model.Migration {
	var out []model.Migration

	for _, src := range migrationSources {
		if !hasTable(src.table) {
			continue
		}

		cols := make([]string, 0, len(src.columns))
		for _, c := range src.columns {
			cols = append(cols, quote(c))
		}
		query := fmt.Sprintf("SELECT %s FROM %s ORDER BY %s LIMIT %d",
			strings.Join(cols, ", "), quote(src.table), quote(src.order), historyLimit)

		rows, err := read(ctx, query)
		if err != nil {
			// Nearly always a column this tool's older schema did not have.
			// Nothing to report: the table simply is not the one we know.
			continue
		}
		for _, r := range rows {
			out = append(out, src.row(r))
		}
	}

	// Entries that carry a time are ordered by it, and everything else keeps
	// the order its table gave. Mixing two tools is rare and sorting the whole
	// list by a version string across tools would be meaningless.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].AppliedAt, out[j].AppliedAt
		if a == nil || b == nil {
			return false
		}
		return a.Before(*b)
	})
	return out
}

func asString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	case time.Time:
		return t.Format(time.RFC3339)
	default:
		return fmt.Sprint(t)
	}
}

func asBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case int64:
		return t != 0
	case int:
		return t != 0
	case string:
		return t == "t" || t == "true" || t == "1"
	default:
		return false
	}
}

func asTime(v any) *time.Time {
	switch t := v.(type) {
	case time.Time:
		return &t
	case *time.Time:
		return t
	case string:
		// SQLite has no time type: what comes back is whatever the tool wrote.
		for _, layout := range []string{
			time.RFC3339Nano, time.RFC3339,
			"2006-01-02 15:04:05.999999999-07:00",
			"2006-01-02 15:04:05",
			"2006-01-02",
		} {
			if parsed, err := time.Parse(layout, t); err == nil {
				return &parsed
			}
		}
	}
	return nil
}

func ifTrue(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}
