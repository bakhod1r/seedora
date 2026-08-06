package ddl

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bakhod1r/seedora/internal/model"
)

// Reading a project's migrations is how Seedora gets pointed at a schema that
// does not exist in any database yet.
//
// The normal path is introspection: connect, read the catalog, seed. That only
// works once somebody has run the migrations. A checkout, a fresh container, or
// a branch that adds three tables all present the same problem — the schema is
// in the repository, as .sql files, and the database is behind it.
//
// So this reads the files, replays them into a schema, and hands back what the
// database is missing. It is not a migration runner and does not pretend to be
// one: it does not record what it applied, it does not run down migrations, and
// a file it cannot read is skipped rather than fatal. What it is for is getting
// tables that exist on disk into a development database so they can be seeded.

// Migration is one file that was read.
type Migration struct {
	// Path is the file, as given.
	Path string `json:"path"`
	// Tables are the tables it creates.
	Tables []string `json:"tables"`
}

// Scan reads a project's migrations and returns the schema they describe.
//
// The path may be a directory or a single .sql file. Files are read in
// lexicographic order, which is what every migration tool's naming convention
// produces: a timestamp or a sequence number in front of the name is the whole
// point of the convention.
//
// Down migrations are skipped. They are recognised by the two conventions in
// use — a separate file (`0003_add_orders.down.sql`, or anything under a
// `down/` directory) and a marker inside an up file (`-- +goose Down`,
// `-- migrate:down`), which is cut at.
func Scan(path string) ([]Change, []Migration, error) {
	files, err := sqlFiles(path)
	if err != nil {
		return nil, nil, err
	}
	if len(files) == 0 {
		return nil, nil, fmt.Errorf("no .sql files under %s", path)
	}

	var (
		order  []string
		byName = map[string]*Change{}
		read   []Migration
	)

	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			return nil, nil, err
		}
		text := cutDown(string(body))

		created, err := Parse(text)
		if err != nil {
			// A file with no CREATE TABLE is ordinary: an index, a data
			// backfill, a grant. It still gets its ALTERs replayed below.
			created = nil
		}

		var made []string
		for _, c := range created {
			if _, seen := byName[c.Table]; !seen {
				order = append(order, c.Table)
			}
			cp := c
			byName[c.Table] = &cp
			made = append(made, c.Table)
		}

		// Later files usually alter what earlier ones created, and a schema
		// replayed without those is wrong in exactly the places a recent branch
		// changed.
		for _, alt := range parseAlters(text) {
			t := byName[alt.Table]
			switch {
			case alt.Kind == DropTable:
				if t != nil {
					delete(byName, alt.Table)
					order = remove(order, alt.Table)
				}
			case t == nil:
				// An ALTER on a table these files never create — it lives in an
				// older migration that is not here, or in the database already.
			case alt.Kind == AddColumn:
				for _, col := range alt.Columns {
					if !hasColumn(*t, col.Name) {
						t.Columns = append(t.Columns, col)
					}
				}
			case alt.Kind == DropColumn:
				t.Columns = withoutColumn(t.Columns, alt.Column)
			}
		}

		if len(made) > 0 || len(parseAlters(text)) > 0 {
			read = append(read, Migration{Path: f, Tables: made})
		}
	}

	out := make([]Change, 0, len(order))
	for _, name := range order {
		if c := byName[name]; c != nil {
			out = append(out, *c)
		}
	}
	if len(out) == 0 {
		return nil, read, fmt.Errorf("no CREATE TABLE statements in %s", path)
	}
	return out, read, nil
}

// Missing splits a scanned schema against the live one: the tables the database
// does not have, and the names of the ones it already does.
//
// This is the answer to the question somebody scanning a migrations directory
// is actually asking — which of these am I missing — and keeping the existing
// names is what lets the UI say "eight already here, three new" rather than
// silently doing nothing about eight of them.
func Missing(s *model.Schema, changes []Change) (missing []Change, existing []string) {
	for _, c := range changes {
		if s != nil && s.Table(c.Table) != nil {
			existing = append(existing, c.Table)
			continue
		}
		missing = append(missing, c)
	}
	return missing, existing
}

// sqlFiles lists the .sql files to read, in the order to read them.
func sqlFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{path}, nil
	}

	var out []string
	err = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if isDownDir(d.Name()) && p != path {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(p), ".sql") && !isDownFile(filepath.Base(p)) {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Lexicographic order, which is what a timestamped or numbered filename is
	// for. Sorting the full path keeps a directory's files together.
	sort.Strings(out)
	return out, nil
}

func isDownDir(name string) bool {
	switch strings.ToLower(name) {
	case "down", "rollback", "revert":
		return true
	}
	return false
}

func isDownFile(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, ".down.") || strings.Contains(n, "_down.") ||
		strings.Contains(n, ".rollback.") || strings.Contains(n, ".undo.")
}

// downMarkers are how the tools that keep both directions in one file separate
// them. Everything after one is the undo, and replaying it would delete the
// tables the file just created.
var downMarkers = []string{
	"+goose down",
	"-- migrate:down",
	"--migrate:down",
	"+migrate down",
	"---- create above / drop below ----",
}

func cutDown(text string) string {
	lower := strings.ToLower(text)
	cut := len(text)
	for _, m := range downMarkers {
		if i := strings.Index(lower, m); i >= 0 && i < cut {
			cut = i
		}
	}
	return text[:cut]
}

// parseAlters reads the ALTER TABLE and DROP TABLE statements a replay needs.
// Anything else — an index, a rename, a constraint — is ignored, the same way
// Parse ignores what it cannot act on.
func parseAlters(sql string) []Change {
	var out []Change

	for _, stmt := range statements(stripComments(sql)) {
		s := strings.TrimSpace(stmt)
		lower := strings.ToLower(s)

		if strings.HasPrefix(lower, "drop table") {
			rest := strings.TrimSpace(s[len("drop table"):])
			rest = trimPrefixFold(rest, "if exists")
			name := unquote(firstWord(rest))
			if name != "" {
				out = append(out, Change{Kind: DropTable, Table: name})
			}
			continue
		}

		if !strings.HasPrefix(lower, "alter table") {
			continue
		}
		rest := strings.TrimSpace(s[len("alter table"):])
		rest = trimPrefixFold(rest, "if exists")
		table := unquote(firstWord(rest))
		rest = strings.TrimSpace(rest[len(firstWord(rest)):])
		lower = strings.ToLower(rest)

		switch {
		case strings.HasPrefix(lower, "add column"), strings.HasPrefix(lower, "add "):
			def := strings.TrimSpace(rest[len("add"):])
			def = trimPrefixFold(def, "column")
			def = trimPrefixFold(def, "if not exists")
			col, ok := parseColumn(def)
			if !ok {
				continue
			}
			out = append(out, Change{Kind: AddColumn, Table: table, Columns: []Column{col}})

		case strings.HasPrefix(lower, "drop column"):
			name := unquote(firstWord(strings.TrimSpace(rest[len("drop column"):])))
			if name != "" {
				out = append(out, Change{Kind: DropColumn, Table: table, Column: name})
			}
		}
	}
	return out
}

func trimPrefixFold(s, prefix string) string {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return strings.TrimSpace(s[len(prefix):])
	}
	return s
}

func firstWord(s string) string {
	s = strings.TrimSpace(s)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '(', ';', ',':
			return s[:i]
		}
	}
	return s
}

func hasColumn(c Change, name string) bool {
	for _, col := range c.Columns {
		if strings.EqualFold(col.Name, name) {
			return true
		}
	}
	return false
}

func withoutColumn(cols []Column, name string) []Column {
	out := cols[:0]
	for _, c := range cols {
		if !strings.EqualFold(c.Name, name) {
			out = append(out, c)
		}
	}
	return out
}

func remove(list []string, name string) []string {
	out := list[:0]
	for _, s := range list {
		if s != name {
			out = append(out, s)
		}
	}
	return out
}
