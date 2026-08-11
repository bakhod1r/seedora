package ui_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bakhod1r/seedora/internal/config"
	"github.com/bakhod1r/seedora/internal/db"
	_ "github.com/bakhod1r/seedora/internal/db/sqlite"
	"github.com/bakhod1r/seedora/internal/plan"
	"github.com/bakhod1r/seedora/internal/spec"
	"github.com/bakhod1r/seedora/internal/ui"
)

// The API is exercised against a real SQLite file rather than a mock driver.
// The routes exist to move a live database from one shape to another, and a
// fake that agrees with the code proves none of that.
func newServer(t *testing.T, ddls ...string) http.Handler {
	t.Helper()
	return newServerBoundTo(t, "127.0.0.1", ddls...)
}

// newServerBoundTo is newServer with the bind address spelled out, for the
// tests that care what the origin guard does with it.
func newServerBoundTo(t *testing.T, host string, ddls ...string) http.Handler {
	t.Helper()

	dir := t.TempDir()
	// The per-machine store — remembered connections and the log of applied
	// schema changes — is redirected into the test's own directory. A test that
	// writes into the developer's real config directory is a test that changes
	// the machine it runs on.
	t.Setenv("SEEDORA_CONFIG_DIR", filepath.Join(dir, "config"))
	dsn := filepath.Join(dir, "test.db")

	ctx := t.Context()
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close(context.Background()) })

	if len(ddls) > 0 {
		tx, err := d.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range ddls {
			if err := tx.Exec(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	sc, err := d.Introspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		ConfigPath: filepath.Join(dir, "seedora.yaml"),
		Locale:     "en_US",
		Host:       host,
		Port:       7777,
	}
	p, loaded, err := spec.LoadOrInfer(cfg.ConfigPath, sc)
	if err != nil {
		t.Fatal(err)
	}
	return ui.New(cfg, d, dsn, sc, p, loaded).Handler()
}

func do(t *testing.T, h http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	// What the page itself sends. httptest's default Host is example.com,
	// which the origin guard refuses — correctly, since a request addressed to
	// a name that is not loopback is the shape a DNS-rebinding attack has.
	req.Host = "127.0.0.1:7777"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", "http://127.0.0.1:7777")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	var out map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: response is not JSON: %s", method, path, w.Body.String())
		}
	}
	return w.Code, out
}

const usersDDL = `CREATE TABLE users (
	id INTEGER PRIMARY KEY,
	email TEXT NOT NULL
)`

func TestStateCarriesDialectAndTypes(t *testing.T) {
	h := newServer(t, usersDDL)

	code, body := do(t, h, "GET", "/api/state", nil)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	if body["dialect"] != "sqlite" {
		t.Errorf("want the sqlite dialect, got %v", body["dialect"])
	}
	// The schema editor cannot offer a type list it was not given.
	types, _ := body["types"].([]any)
	if len(types) == 0 {
		t.Error("want the dialect's column types")
	}
}

// The plan route renders SQL and runs nothing. That is the whole reason it is
// separate from apply.
func TestSchemaPlanRunsNothing(t *testing.T) {
	h := newServer(t, usersDDL)

	code, body := do(t, h, "POST", "/api/schema/plan", map[string]any{
		"changes": []map[string]any{{
			"kind":  "create_table",
			"table": "orders",
			"columns": []map[string]any{
				{"name": "id", "type": "INTEGER", "pk": true},
				{"name": "user_id", "type": "INTEGER", "references": "users.id"},
			},
		}},
	})
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, body)
	}
	sql, _ := body["sql"].([]any)
	if len(sql) != 1 || !strings.Contains(sql[0].(string), `CREATE TABLE "orders"`) {
		t.Fatalf("want a CREATE TABLE, got %v", body["sql"])
	}

	// Nothing was created: the table must still be absent from the schema.
	_, st := do(t, h, "GET", "/api/state", nil)
	if tableNames(st)["orders"] {
		t.Fatal("plan created the table; it must only render SQL")
	}
}

func TestSchemaPlanReportsEveryProblem(t *testing.T) {
	h := newServer(t, usersDDL)

	code, body := do(t, h, "POST", "/api/schema/plan", map[string]any{
		"changes": []map[string]any{{
			"kind":  "create_table",
			"table": "orders",
			"columns": []map[string]any{
				{"name": "1st", "type": ""},
				{"name": "user_id", "type": "INTEGER", "references": "ghosts.id"},
			},
		}},
	})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d: %v", code, body)
	}
	problems, _ := body["problems"].([]any)
	if len(problems) < 3 {
		t.Fatalf("want every problem reported, got %v", problems)
	}
}

// Apply runs the statements, re-introspects, and hands back a state that
// describes the database as it now is — including a plan for the new table.
func TestSchemaApplyCreatesTable(t *testing.T) {
	h := newServer(t, usersDDL)

	code, body := do(t, h, "POST", "/api/schema/apply", map[string]any{
		"changes": []map[string]any{{
			"kind":  "create_table",
			"table": "orders",
			"columns": []map[string]any{
				{"name": "id", "type": "INTEGER", "pk": true},
				{"name": "user_id", "type": "INTEGER", "nullable": true, "references": "users.id"},
				{"name": "total", "type": "DECIMAL(10,2)"},
			},
		}},
	})
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, body)
	}
	if !tableNames(body)["orders"] {
		t.Fatal("the new table is missing from the returned schema")
	}

	p, ok := body["plan"].(map[string]any)
	if !ok {
		t.Fatal("no plan in the response")
	}
	tables, _ := p["tables"].(map[string]any)
	if _, ok := tables["orders"]; !ok {
		t.Fatal("the new table has no plan, so the page cannot seed it")
	}

	// The foreign key is real, not just spelled: it must have been introspected
	// back off the live catalog.
	if !hasFK(body, "orders", "user_id") {
		t.Error("the foreign key did not survive the round trip")
	}
}

func TestSchemaApplyAddsAndDropsColumns(t *testing.T) {
	h := newServer(t, usersDDL)

	code, body := do(t, h, "POST", "/api/schema/apply", map[string]any{
		"changes": []map[string]any{
			{"kind": "add_column", "table": "users",
				"columns": []map[string]any{{"name": "nick", "type": "TEXT", "nullable": true}}},
			{"kind": "drop_column", "table": "users", "column": "email"},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, body)
	}
	cols := columnNames(body, "users")
	if !cols["nick"] {
		t.Error("the added column is missing")
	}
	if cols["email"] {
		t.Error("the dropped column is still there")
	}
}

// A batch is one transaction: a statement that fails must leave the schema
// exactly as it was, not half-edited.
func TestSchemaApplyIsAtomic(t *testing.T) {
	h := newServer(t, usersDDL)

	// The first change is fine; the second names a type SQLite will not parse,
	// so the batch fails at the second statement.
	code, body := do(t, h, "POST", "/api/schema/apply", map[string]any{
		"changes": []map[string]any{
			{"kind": "create_table", "table": "orders",
				"columns": []map[string]any{{"name": "id", "type": "INTEGER", "pk": true}}},
			{"kind": "create_table", "table": "carts",
				"columns": []map[string]any{{"name": "id", "type": "INTEGER PRIMARY KEY GARBAGE(", "pk": true}}},
		},
	})
	if code == http.StatusOK {
		t.Fatalf("want a failure, got 200: %v", body)
	}

	_, st := do(t, h, "GET", "/api/state", nil)
	names := tableNames(st)
	if names["orders"] || names["carts"] {
		t.Fatalf("a failed batch left tables behind: %v", names)
	}
}

func TestSchemaApplyRejectsInvalidChanges(t *testing.T) {
	h := newServer(t, usersDDL)

	// Dropping a primary key column is refused before any SQL is rendered.
	code, _ := do(t, h, "POST", "/api/schema/apply", map[string]any{
		"changes": []map[string]any{{"kind": "drop_column", "table": "users", "column": "id"}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", code)
	}

	code, _ = do(t, h, "POST", "/api/schema/apply", map[string]any{"changes": []map[string]any{}})
	if code != http.StatusBadRequest {
		t.Fatalf("an empty batch should be refused, got %d", code)
	}
}

// The mapping is the work the tool exists for. A schema change must not throw
// it away.
func TestSchemaApplyKeepsExistingGenerators(t *testing.T) {
	h := newServer(t, usersDDL)

	_, st := do(t, h, "GET", "/api/state", nil)
	p, _ := st["plan"].(map[string]any)
	tables, _ := p["tables"].(map[string]any)
	users, _ := tables["users"].(map[string]any)
	cols, _ := users["columns"].(map[string]any)
	email, _ := cols["email"].(map[string]any)
	email["generator"] = "full_name"
	users["rows"] = float64(4242)

	if code, body := do(t, h, "PUT", "/api/plan", p); code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, body)
	}

	code, body := do(t, h, "POST", "/api/schema/apply", map[string]any{
		"changes": []map[string]any{{
			"kind": "add_column", "table": "users",
			"columns": []map[string]any{{"name": "nick", "type": "TEXT", "nullable": true}},
		}},
	})
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, body)
	}

	after, _ := body["plan"].(map[string]any)
	at, _ := after["tables"].(map[string]any)
	au, _ := at["users"].(map[string]any)
	ac, _ := au["columns"].(map[string]any)
	ae, _ := ac["email"].(map[string]any)
	if ae == nil || ae["generator"] != "full_name" {
		t.Errorf("the chosen generator was lost: %v", ae)
	}
	if au["rows"] != float64(4242) {
		t.Errorf("the chosen row count was lost: %v", au["rows"])
	}
	// The new column still needs a proposal of its own.
	if _, ok := ac["nick"]; !ok {
		t.Error("the added column got no generator")
	}
}

// Column order is the user's: it is what they dragged the card into. The server
// reconciles it with the schema rather than overwriting it, or a reorder would
// last exactly until the next request.
func TestColumnOrderSurvivesAndReconciles(t *testing.T) {
	h := newServer(t, usersDDL)

	_, st := do(t, h, "GET", "/api/state", nil)
	p, _ := st["plan"].(map[string]any)
	tables, _ := p["tables"].(map[string]any)
	users, _ := tables["users"].(map[string]any)
	users["order"] = []any{"email", "id"}

	code, body := do(t, h, "PUT", "/api/plan", p)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, body)
	}
	if got := planOrder(body, "users"); !slices.Equal(got, []string{"email", "id"}) {
		t.Fatalf("the chosen order was not kept: %v", got)
	}

	// A column the database gains is appended rather than resorting the rest.
	code, body = do(t, h, "POST", "/api/schema/apply", map[string]any{
		"changes": []map[string]any{{
			"kind": "add_column", "table": "users",
			"columns": []map[string]any{{"name": "nick", "type": "TEXT", "nullable": true}},
		}},
	})
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, body)
	}
	if got := planOrder(body, "users"); !slices.Equal(got, []string{"email", "id", "nick"}) {
		t.Fatalf("want the new column appended, got %v", got)
	}

	// A column it loses drops out of the order with it.
	code, body = do(t, h, "POST", "/api/schema/apply", map[string]any{
		"changes": []map[string]any{{"kind": "drop_column", "table": "users", "column": "email"}},
	})
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, body)
	}
	if got := planOrder(body, "users"); !slices.Equal(got, []string{"id", "nick"}) {
		t.Fatalf("want the dropped column gone, got %v", got)
	}
}

func planOrder(state map[string]any, table string) []string {
	p, _ := state["plan"].(map[string]any)
	tables, _ := p["tables"].(map[string]any)
	tp, _ := tables[table].(map[string]any)
	raw, _ := tp["order"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

func TestPreviewRegeneratesWithANonce(t *testing.T) {
	h := newServer(t, usersDDL)

	code, first := do(t, h, "POST", "/api/preview",
		map[string]any{"table": "users", "rows": 5})
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, first)
	}

	// The same request twice is the same rows: opening a table shows a stable
	// preview, so a change in it is a change the user made.
	_, again := do(t, h, "POST", "/api/preview",
		map[string]any{"table": "users", "rows": 5})
	if !sameRows(first, again) {
		t.Error("a preview with no nonce should be stable")
	}

	// Regenerate sends a nonce, and must actually produce different rows.
	_, fresh := do(t, h, "POST", "/api/preview",
		map[string]any{"table": "users", "rows": 5, "nonce": 99})
	if sameRows(first, fresh) {
		t.Error("Regenerate produced the same rows, so the button does nothing")
	}
}

// Every route that needs a connection must say so rather than panicking on a
// nil driver.
// disconnected is the server as it is before anything has been connected,
// which every route that needs a database has to survive.
func disconnected(t *testing.T) http.Handler {
	t.Helper()
	cfg := &config.Config{
		ConfigPath: filepath.Join(t.TempDir(), "seedora.yaml"),
		Host:       "127.0.0.1",
		Port:       7777,
	}
	return ui.New(cfg, nil, "", nil, (*plan.Plan)(nil), false).Handler()
}

func TestRoutesWithoutAConnection(t *testing.T) {
	h := disconnected(t)

	cases := []struct {
		path string
		body map[string]any
	}{
		{"/api/preview", map[string]any{"table": "users"}},
		{"/api/validate", map[string]any{}},
		{"/api/schema/plan", map[string]any{
			"changes": []map[string]any{{"kind": "drop_table", "table": "users"}}}},
		{"/api/schema/apply", map[string]any{
			"changes": []map[string]any{{"kind": "drop_table", "table": "users"}}}},
	}
	for _, c := range cases {
		code, _ := do(t, h, "POST", c.path, c.body)
		if code != http.StatusConflict {
			t.Errorf("%s: want 409, got %d", c.path, code)
		}
	}
}

func tableNames(state map[string]any) map[string]bool {
	out := map[string]bool{}
	sc, _ := state["schema"].(map[string]any)
	tables, _ := sc["tables"].([]any)
	for _, t := range tables {
		tm, _ := t.(map[string]any)
		name, _ := tm["name"].(string)
		out[name] = true
	}
	return out
}

func columnNames(state map[string]any, table string) map[string]bool {
	out := map[string]bool{}
	for _, c := range columnsOf(state, table) {
		name, _ := c["name"].(string)
		out[name] = true
	}
	return out
}

func hasFK(state map[string]any, table, column string) bool {
	for _, c := range columnsOf(state, table) {
		if c["name"] == column && c["fk"] != nil {
			return true
		}
	}
	return false
}

func columnsOf(state map[string]any, table string) []map[string]any {
	sc, _ := state["schema"].(map[string]any)
	tables, _ := sc["tables"].([]any)
	for _, t := range tables {
		tm, _ := t.(map[string]any)
		if tm["name"] != table {
			continue
		}
		cols, _ := tm["columns"].([]any)
		out := make([]map[string]any, 0, len(cols))
		for _, c := range cols {
			cm, _ := c.(map[string]any)
			out = append(out, cm)
		}
		return out
	}
	return nil
}

func sameRows(a, b map[string]any) bool {
	x, _ := json.Marshal(a["rows"])
	y, _ := json.Marshal(b["rows"])
	return bytes.Equal(x, y)
}
