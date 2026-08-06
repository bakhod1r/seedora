package ui

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
	"github.com/bakhod1r/seedora/internal/plan"
	"github.com/bakhod1r/seedora/internal/spec"
	"github.com/bakhod1r/seedora/internal/store"
)

// The schema editor is two routes on purpose. Plan renders the SQL and runs
// nothing, so the page can show exactly what will happen; Apply runs that same
// SQL in one transaction. Nothing is applied that was not previewable first.

type schemaRequest struct {
	Changes []ddl.Change `json:"changes"`
}

// handleSchemaPlan validates a batch of edits and returns the SQL they render
// to. It touches the database not at all.
//
// Every problem is reported, not just the first: a schema sketched in the
// diagram usually has several gaps at once, and one per round trip is the
// slowest way to find that out.
func (s *Server) handleSchemaPlan(w http.ResponseWriter, r *http.Request) {
	var req schemaRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	s.mu.Lock()
	d, sc := s.driver, s.schema
	s.mu.Unlock()
	if d == nil || sc == nil {
		writeErr(w, http.StatusConflict, errNotConnected)
		return
	}
	if len(req.Changes) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("no changes"))
		return
	}

	dialect := d.Dialect()
	if problems := ddl.Validate(dialect, sc, req.Changes); len(problems) > 0 {
		msgs := make([]string, 0, len(problems))
		for _, p := range problems {
			msgs = append(msgs, p.Error())
		}
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":    "these changes cannot be applied",
			"problems": msgs,
		})
		return
	}

	stmts, err := ddl.Plan(dialect, sc, req.Changes)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sql": stmts})
}

// handleSchemaApply runs the same statements handleSchemaPlan renders, in one
// transaction, then re-introspects and re-infers so the page sees the database
// as it now is rather than as the client guessed it would be.
func (s *Server) handleSchemaApply(w http.ResponseWriter, r *http.Request) {
	var req schemaRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	s.mu.Lock()
	d, sc, running := s.driver, s.schema, s.running
	s.mu.Unlock()
	if d == nil || sc == nil {
		writeErr(w, http.StatusConflict, errNotConnected)
		return
	}
	// A seeding run holds its own transaction open against these tables, and
	// altering them underneath it is how a run fails halfway.
	if running {
		writeErr(w, http.StatusConflict, errRunning)
		return
	}
	if len(req.Changes) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("no changes"))
		return
	}

	stmts, err := ddl.Plan(d.Dialect(), sc, req.Changes)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := applyStatements(r.Context(), d, stmts); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	// Recorded before the re-introspection, because what was run is worth
	// keeping even if reading the schema back fails. A migration tool's table
	// will never mention this change — Seedora does not write into anyone
	// else's bookkeeping — so this log is the only record that it happened.
	if err := store.RecordApplied(s.dsn, summarise(req.Changes), stmts); err != nil {
		// A log that cannot be written is not a reason to fail a change that
		// has already been committed.
		log.Printf("seedora: could not record the applied change: %v", err)
	}

	// The catalog is the authority on what the changes produced — defaults,
	// implied indexes, the type the engine settled on — so the new plan is
	// inferred from a fresh read rather than patched onto the old one.
	fresh, err := d.Introspect(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.schema = fresh
	if s.plan != nil {
		// Keep every generator the user chose for a table that still exists,
		// and infer only what is new. Re-inferring wholesale would throw away
		// the mapping work that is the point of the tool.
		merged := plan.Infer(fresh)
		s.plan.Merge(merged)
		restoreOrder(s.plan, fresh)
	} else {
		p, loaded, err := spec.LoadOrInfer(s.cfg.ConfigPath, fresh)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		s.plan, s.loaded = p, loaded
	}
	writeJSON(w, http.StatusOK, s.snapshot())
}

// applyStatements runs the batch in one transaction, so a failure on the third
// statement leaves the schema exactly as it was rather than half-edited.
func applyStatements(ctx context.Context, d db.Driver, stmts []string) error {
	tx, err := d.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	for _, stmt := range stmts {
		if err := tx.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

// summarise describes a batch in one line, for the history list. The SQL is
// kept alongside it, so this only has to be enough to recognise the entry.
func summarise(changes []ddl.Change) string {
	counts := map[ddl.Kind]int{}
	tables := map[string]bool{}
	for _, c := range changes {
		counts[c.Kind]++
		tables[c.Table] = true
	}

	var parts []string
	for _, k := range []struct {
		kind ddl.Kind
		one  string
		many string
	}{
		{ddl.CreateTable, "created 1 table", "created %d tables"},
		{ddl.AddColumn, "added 1 column", "added %d columns"},
		{ddl.DropColumn, "dropped 1 column", "dropped %d columns"},
		{ddl.DropTable, "dropped 1 table", "dropped %d tables"},
	} {
		n := counts[k.kind]
		if n == 1 {
			parts = append(parts, k.one)
		} else if n > 1 {
			parts = append(parts, fmt.Sprintf(k.many, n))
		}
	}
	if len(parts) == 0 {
		return "no changes"
	}

	names := make([]string, 0, len(tables))
	for t := range tables {
		names = append(names, t)
	}
	sort.Strings(names)
	if len(names) > 3 {
		names = append(names[:3], "…")
	}
	return strings.Join(parts, ", ") + " · " + strings.Join(names, ", ")
}

// handleHistory answers "how did this schema get like this".
//
// Two sources, and the difference between them matters. The database's own
// answer is whatever a migration tool wrote into its bookkeeping table — the
// engine keeps no DDL history, so there is nothing else to read. Seedora's
// answer is the changes it applied itself, which no migration tool knows about.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	d, dsn := s.driver, s.dsn
	s.mu.Unlock()
	if d == nil {
		writeErr(w, http.StatusConflict, errNotConnected)
		return
	}

	migrations, err := d.History(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	for _, e := range store.AppliedFor(dsn) {
		at := e.At
		applied := true
		migrations = append(migrations, model.Migration{
			Source:     "seedora",
			Version:    at.Format("2006-01-02 15:04"),
			Name:       e.Summary,
			AppliedAt:  &at,
			Applied:    &applied,
			Statements: e.Statements,
		})
	}

	// Newest first: the question is nearly always about the last thing that
	// happened.
	sort.SliceStable(migrations, func(i, j int) bool {
		a, b := migrations[i].AppliedAt, migrations[j].AppliedAt
		if a == nil || b == nil {
			return b == nil && a != nil
		}
		return a.After(*b)
	})
	writeJSON(w, http.StatusOK, map[string]any{"entries": migrations})
}

// handleSchemaScan reads a project's migration files and reports what the
// database is missing.
//
// This is the answer to a state every project is in at least once a week: the
// schema lives in the repository as .sql files, somebody checked out a branch
// that adds three tables, and the local database is behind. Introspection
// cannot see those tables because they do not exist yet, so there is nothing to
// map and nothing to seed.
//
// Nothing is applied here. The scan returns the missing tables as ordinary
// schema-editor changes and the SQL they render to, which is the same review
// step every other edit goes through — the diagram draws them, the dialog shows
// the statements, and Apply is a decision the user makes.
func (s *Server) handleSchemaScan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	path := strings.TrimSpace(req.Path)
	if path == "" {
		path = s.cfg.Migrations
	}
	if path == "" {
		writeErr(w, http.StatusBadRequest, errors.New("no path: give a migrations directory or a .sql file"))
		return
	}

	changes, files, err := ddl.Scan(path)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err)
		return
	}

	s.mu.Lock()
	d, sc := s.driver, s.schema
	s.mu.Unlock()

	missing, existing := ddl.Missing(sc, changes)
	sort.Strings(existing)

	out := map[string]any{
		"path":     path,
		"files":    files,
		"tables":   len(changes),
		"changes":  missing,
		"existing": existing,
	}
	// The SQL is rendered when there is a connection to render it for: which
	// dialect to write is the driver's answer, and there is no sensible default
	// to guess at.
	if d != nil && len(missing) > 0 {
		if problems := ddl.Validate(d.Dialect(), sc, missing); len(problems) > 0 {
			msgs := make([]string, 0, len(problems))
			for _, p := range problems {
				msgs = append(msgs, p.Error())
			}
			out["problems"] = msgs
		} else if stmts, err := ddl.Plan(d.Dialect(), sc, missing); err == nil {
			out["sql"] = stmts
		}
	}
	writeJSON(w, http.StatusOK, out)
}
