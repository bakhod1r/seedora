// Package ui serves the mapping editor: a schema diagram where every column
// carries the generator that will fill it, and every one of those is editable.
//
// The page is a single embedded HTML file with no build step and no runtime
// dependency, which is what lets Seedora ship as one binary.
package ui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bakhod1r/seedora/internal/config"
	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
	"github.com/bakhod1r/seedora/internal/plan"
	"github.com/bakhod1r/seedora/internal/seed"
	"github.com/bakhod1r/seedora/internal/spec"
	"github.com/bakhod1r/seedora/internal/store"
)

//go:embed assets
var assets embed.FS

// Server holds the session the page edits. There is exactly one: Seedora is a
// local tool a developer runs against their own database, and pretending
// otherwise would mean authentication, per-user state, and a much larger thing
// than the job needs.
type Server struct {
	cfg *config.Config

	mu     sync.Mutex
	driver db.Driver
	dsn    string
	schema *model.Schema
	plan   *plan.Plan
	// loaded records whether the plan came from a file, which is what the page
	// uses to decide between "Save" and "Save as seedora.yaml".
	loaded  bool
	running bool
	last    *seed.Result
	// run is the seeding run in flight, or the last one to finish. The page
	// watches it over SSE rather than holding a request open for its duration.
	run *run
}

// New returns a server. A nil driver means the page opens on the connect screen.
func New(cfg *config.Config, d db.Driver, dsn string, s *model.Schema, p *plan.Plan, loaded bool) *Server {
	return &Server{cfg: cfg, driver: d, dsn: dsn, schema: s, plan: p, loaded: loaded}
}

// Handler builds the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err) // Only possible if the embed directive and the path disagree.
	}
	// The page, the script, and the stylesheet are one unit: a browser holding
	// an old copy of one and a new copy of another produces a page whose script
	// dies on an element that is not there. Embedded files carry no modification
	// time, so without this a heuristically-cached asset can outlive an upgrade.
	mux.Handle("GET /", noCache(http.FileServerFS(sub)))

	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/generators", s.handleGenerators)
	mux.HandleFunc("GET /api/connections", s.handleConnections)
	mux.HandleFunc("POST /api/connections/forget", s.handleForget)
	mux.HandleFunc("POST /api/connect", s.handleConnect)
	mux.HandleFunc("PUT /api/plan", s.handlePlan)
	mux.HandleFunc("POST /api/validate", s.handleValidate)
	mux.HandleFunc("POST /api/preview", s.handlePreview)
	mux.HandleFunc("POST /api/schema/plan", s.handleSchemaPlan)
	mux.HandleFunc("POST /api/schema/apply", s.handleSchemaApply)
	mux.HandleFunc("POST /api/schema/scan", s.handleSchemaScan)
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("POST /api/seed", s.handleSeed)
	mux.HandleFunc("GET /api/seed/events", s.handleSeedEvents)
	mux.HandleFunc("POST /api/save", s.handleSave)
	mux.HandleFunc("GET /api/export", s.handleExport)
	mux.HandleFunc("POST /api/import", s.handleImport)

	return mux
}

// noCache makes the browser revalidate the page assets on every load. They are
// served from memory by a local process, so the check costs nothing and a stale
// mix of them costs a broken page.
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}

var (
	errNotConnected = errors.New("not connected")
	errRunning      = errors.New("a run is already in progress")
	errNoRun        = errors.New("no run has been started")
	errNoStreaming  = errors.New("this server cannot stream")
)

// state is the whole page payload. It is sent in one response rather than in
// four, because every part of the page needs a consistent view of the same
// schema and plan.
type state struct {
	Connected bool          `json:"connected"`
	Engine    string        `json:"engine,omitempty"`
	Target    string        `json:"target,omitempty"`
	Schema    *model.Schema `json:"schema,omitempty"`
	Plan      *plan.Plan    `json:"plan,omitempty"`
	// ConfigPath is where Save writes.
	ConfigPath string   `json:"config_path"`
	Loaded     bool     `json:"loaded"`
	Running    bool     `json:"running"`
	Problems   []string `json:"problems,omitempty"`
	// Dialect and Types drive the schema editor: which SQL is rendered, and
	// which column types the diagram offers.
	Dialect ddl.Dialect  `json:"dialect,omitempty"`
	Types   []string     `json:"types,omitempty"`
	Last    *seed.Result `json:"last,omitempty"`
	// Migrations is the project's migration directory, when one was given on
	// the command line. The page scans it on load, so a checkout whose database
	// is behind its repository opens with the missing tables already drawn.
	Migrations string `json:"migrations,omitempty"`
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.snapshot())
}

// snapshot must be called with the lock held.
func (s *Server) snapshot() state {
	st := state{
		Connected:  s.driver != nil,
		ConfigPath: s.cfg.ConfigPath,
		Loaded:     s.loaded,
		Running:    s.running,
		Schema:     s.schema,
		Plan:       s.plan,
		Last:       s.last,
		Migrations: s.cfg.Migrations,
	}
	if s.driver != nil {
		st.Engine = s.driver.Name()
		st.Dialect = s.driver.Dialect()
		st.Types = ddl.Types(st.Dialect)
		// The redacted form, never the DSN: the page is HTML in a browser and
		// the DSN carries a password.
		st.Target = config.Redacted(s.dsn)
	}
	if s.schema != nil && s.plan != nil {
		for _, err := range s.plan.Validate(s.schema) {
			st.Problems = append(st.Problems, err.Error())
		}
	}
	return st
}

func (s *Server) handleGenerators(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, Generators())
}

// handleConnections lists the databases this user has connected to before. The
// list is per-machine and lives outside the repository; see internal/store.
func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	f, err := store.Load()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// Masked, always: this response is JSON in a browser, and a stored password
	// has no business being in it.
	out := make([]map[string]any, 0, len(f.Connections))
	for _, c := range f.Connections {
		out = append(out, map[string]any{
			"name":           c.Name,
			"dsn":            c.Redacted(),
			"needs_password": c.HasPassword,
			"last_used":      c.LastUsed,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleForget drops a remembered connection.
func (s *Server) handleForget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	f, err := store.Load()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// Selection is by name rather than by DSN, because the DSN the page holds
	// is the masked one and would not match what is on disk.
	for _, c := range f.Connections {
		if c.Name == req.Name {
			if err := f.Forget(c.DSN); err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			break
		}
	}
	s.handleConnections(w, r)
}

// handleConnect opens a database from the UI's connect screen.
//
// The DSN arrives one of two ways: pasted in full, or named — a remembered
// connection the page identifies by name, whose real DSN never left the disk.
// The second path is what lets the list show masked strings while still being
// clickable.
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DSN string `json:"dsn"`
		// Name selects a remembered connection instead of supplying a DSN.
		Name string `json:"name"`
		// Password fills in the one a remembered connection did not store.
		Password string `json:"password"`
		// Remember adds the connection to the list on success.
		Remember bool `json:"remember"`
		// KeepPassword stores the password too. Off by default, and the UI
		// spells out what it means.
		KeepPassword bool `json:"keep_password"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	dsn := req.DSN
	if req.Name != "" {
		resolved, err := lookupConnection(req.Name)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		dsn = resolved
	}
	if req.Password != "" {
		withPass, err := store.WithPassword(dsn, req.Password)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		dsn = withPass
	}
	if strings.TrimSpace(dsn) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("no connection string given"))
		return
	}

	if err := db.Guard(dsn, s.cfg.AllowProduction); err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	d, err := db.Open(ctx, dsn)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	sc, err := d.Introspect(ctx)
	if err != nil {
		d.Close(ctx)
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	p, loaded, err := spec.LoadOrInfer(s.cfg.ConfigPath, sc)
	if err != nil {
		d.Close(ctx)
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	// The lock is held only long enough to swap the session over, then released
	// before touching the disk — the store write is slow enough that holding a
	// lock across it would block every other request for no reason.
	s.mu.Lock()
	if s.driver != nil {
		_ = s.driver.Close(ctx)
	}
	s.driver, s.dsn, s.schema, s.plan, s.loaded = d, dsn, sc, p, loaded
	s.last = nil
	snap := s.snapshot()
	s.mu.Unlock()

	// Remembered only after a successful connect and introspect, so the list
	// never fills up with strings that do not work. A failure to write it is
	// not a failure to connect: the session is already live.
	if req.Remember {
		if f, err := store.Load(); err == nil {
			_ = f.Remember("", dsn, req.KeepPassword)
		}
	}

	writeJSON(w, http.StatusOK, snap)
}

// lookupConnection resolves a remembered connection's name to the DSN held on
// disk. The page only ever sees the masked form, so this is the only way a
// stored password reaches a connection attempt.
func lookupConnection(name string) (string, error) {
	f, err := store.Load()
	if err != nil {
		return "", err
	}
	for _, c := range f.Connections {
		if c.Name == name {
			return c.DSN, nil
		}
	}
	return "", fmt.Errorf("no remembered connection named %q", name)
}

// handlePlan replaces the plan with the one the page is holding. The whole plan
// is sent rather than a patch: it is a few hundred kilobytes at most, and a
// patch protocol would be the only stateful thing in the server.
func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	var p plan.Plan
	if err := decode(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schema == nil {
		writeErr(w, http.StatusConflict, errNotConnected)
		return
	}
	restoreOrder(&p, s.schema)
	s.plan = &p
	writeJSON(w, http.StatusOK, s.snapshot())
}

func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schema == nil || s.plan == nil {
		writeErr(w, http.StatusConflict, errNotConnected)
		return
	}
	var msgs []string
	for _, err := range s.plan.Validate(s.schema) {
		msgs = append(msgs, err.Error())
	}
	writeJSON(w, http.StatusOK, map[string]any{"problems": msgs})
}

// handlePreview generates a handful of rows for one table without writing
// anything. This is the loop the UI is for: change a generator, see what it
// actually produces, keep it or change it again.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Table string `json:"table"`
		Rows  int    `json:"rows"`
		// Nonce is set by Regenerate to ask for a fresh draw. Zero keeps the
		// stable preview a first open should show.
		Nonce uint64 `json:"nonce"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Rows <= 0 || req.Rows > 50 {
		req.Rows = 5
	}

	s.mu.Lock()
	d, sc, p := s.driver, s.schema, s.plan
	s.mu.Unlock()
	if d == nil || sc == nil || p == nil {
		writeErr(w, http.StatusConflict, errNotConnected)
		return
	}

	rows, cols, err := seed.Preview(r.Context(), d, sc, p, req.Table, req.Rows, s.cfg.Locale, req.Nonce)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"columns": cols, "rows": rows})
}

// handleSave writes the mapping to seedora.yaml.
func (s *Server) handleSave(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.plan == nil {
		writeErr(w, http.StatusConflict, errors.New("nothing to save"))
		return
	}
	if err := spec.Save(s.cfg.ConfigPath, s.plan, s.schema); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.loaded = true
	writeJSON(w, http.StatusOK, map[string]any{"path": s.cfg.ConfigPath})
}

// handleImport takes a seedora.yaml a user pasted or dropped on the page and
// makes it the working plan.
//
// It is merged against the live schema rather than swapped in: an imported file
// was written against some schema, and the one in front of us may have moved on.
// Merging keeps every choice the file made about a column that still exists,
// proposes generators for columns it has never seen, and drops the ones the
// database no longer has — the same rule a re-scan follows.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		YAML string `json:"yaml"`
		// Replace swaps the plan in wholesale instead of merging, for a file the
		// user knows is authoritative.
		Replace bool `json:"replace"`
		// Format is "yaml" for a mapping, "sql" for a schema file. A schema is
		// not a mapping: it says which tables should exist, so it comes back as
		// changes to review rather than being applied here.
		Format string `json:"format"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.YAML) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("nothing to import"))
		return
	}

	if req.Format == "sql" {
		s.importSQL(w, r, req.YAML)
		return
	}

	imported, err := spec.Parse([]byte(req.YAML))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schema == nil {
		writeErr(w, http.StatusConflict, errors.New("connect to a database first"))
		return
	}

	if !req.Replace {
		imported.Merge(plan.Infer(s.schema))
	}
	if imported.Locale == "" {
		imported.Locale = s.cfg.Locale
	}
	restoreOrder(imported, s.schema)

	// An import that does not fit the live schema is reported rather than
	// applied: silently accepting it would leave the page showing a plan that
	// cannot run, and the person would find out only when they pressed Seed.
	if problems := imported.Validate(s.schema); len(problems) > 0 {
		msgs := make([]string, 0, len(problems))
		for _, p := range problems {
			msgs = append(msgs, p.Error())
		}
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":    "the imported config does not fit this database",
			"problems": msgs,
		})
		return
	}

	s.plan = imported
	s.loaded = true
	writeJSON(w, http.StatusOK, s.snapshot())
}

// importSQL reads a schema file and returns the changes it describes, checked
// against the database that is connected. Nothing is applied: a file that
// creates fourteen tables is exactly the case where the SQL should be read
// before it runs, and the schema editor already has the dialog for that.
func (s *Server) importSQL(w http.ResponseWriter, r *http.Request, sql string) {
	s.mu.Lock()
	d, sc := s.driver, s.schema
	s.mu.Unlock()
	if d == nil || sc == nil {
		writeErr(w, http.StatusConflict, errNotConnected)
		return
	}

	changes, err := ddl.Parse(sql)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	// Tables that already exist are dropped from the batch rather than
	// reported: pointing a schema file at the database it came from is the
	// normal way to find out what is missing, and failing the whole import over
	// the tables that are already right would make that impossible.
	var missing []ddl.Change
	var skipped []string
	for _, c := range changes {
		if sc.Table(c.Table) != nil {
			skipped = append(skipped, c.Table)
			continue
		}
		missing = append(missing, c)
	}

	out := map[string]any{"skipped": skipped, "changes": missing}
	if len(missing) == 0 {
		out["sql"] = []string{}
		writeJSON(w, http.StatusOK, out)
		return
	}

	dialect := d.Dialect()
	if problems := ddl.Validate(dialect, sc, missing); len(problems) > 0 {
		msgs := make([]string, 0, len(problems))
		for _, p := range problems {
			msgs = append(msgs, p.Error())
		}
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":    "this schema does not fit the connected database",
			"problems": msgs,
		})
		return
	}

	stmts, err := ddl.Plan(dialect, sc, missing)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out["sql"] = stmts
	writeJSON(w, http.StatusOK, out)
}

// handleExport hands back the current plan as the YAML that would be written to
// seedora.yaml, so it can be downloaded without touching the filesystem — useful
// when Seedora is running somewhere the browser is not.
// Three things are worth taking out of a session, and they are different
// things: the mapping, which is what Seedora knows and nothing else does; the
// schema as SQL, to stand the same tables up elsewhere; and the diagram as a
// Mermaid script, which is the only one of the three that renders in a README.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	format := r.URL.Query().Get("format")
	if format == "" {
		format = "yaml"
	}

	var (
		body        []byte
		contentType = "text/plain; charset=utf-8"
		filename    string
	)

	switch format {
	case "yaml":
		if s.plan == nil {
			writeErr(w, http.StatusConflict, errors.New("nothing to export"))
			return
		}
		b, err := spec.Render(s.plan, s.schema)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		body, contentType, filename = b, "application/yaml; charset=utf-8", "seedora.yaml"

	case "sql":
		if s.schema == nil || s.driver == nil {
			writeErr(w, http.StatusConflict, errNotConnected)
			return
		}
		stmts := ddl.Script(s.driver.Dialect(), s.schema)
		if len(stmts) == 0 {
			writeErr(w, http.StatusConflict, errors.New("this database has no tables"))
			return
		}
		// Semicolons and blank lines, because this file is meant to be read and
		// then run by something that is not this program.
		body = []byte(strings.Join(stmts, ";\n\n") + ";\n")
		contentType, filename = "application/sql; charset=utf-8", "schema.sql"

	case "mermaid":
		if s.schema == nil {
			writeErr(w, http.StatusConflict, errNotConnected)
			return
		}
		body = []byte(ddl.Mermaid(s.schema))
		filename = "schema.mmd"

	default:
		writeErr(w, http.StatusBadRequest,
			fmt.Errorf("unknown format %q — use yaml, sql, or mermaid", format))
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(body)
}

// restoreOrder reconciles the plan's column order with the live schema. The
// order is the user's — columns can be dragged into the arrangement that makes
// the table readable — so it is kept, not overwritten: entries for columns the
// database no longer has are dropped, and columns it has gained are appended in
// catalog order.
//
// The order is display and YAML only. A bulk load still writes columns in the
// order the catalog reports, which is the order the wire protocol expects.
func restoreOrder(p *plan.Plan, s *model.Schema) {
	for name, tp := range p.Tables {
		t := s.Table(name)
		if t == nil {
			continue
		}
		live := map[string]bool{}
		for _, c := range t.Columns {
			live[c.Name] = true
		}

		seen := map[string]bool{}
		kept := tp.Order[:0]
		for _, c := range tp.Order {
			if live[c] && !seen[c] {
				kept = append(kept, c)
				seen[c] = true
			}
		}
		for _, c := range t.Columns {
			if !seen[c.Name] {
				kept = append(kept, c.Name)
				seen[c.Name] = true
			}
		}
		tp.Order = kept
	}
}

// Close releases the database connection.
func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.driver == nil {
		return nil
	}
	err := s.driver.Close(ctx)
	s.driver = nil
	return err
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 32<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("bad request body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// The page is served from the same origin and holds a live database
	// connection; no other origin has any business reading it.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
