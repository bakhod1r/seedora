package ui_test

import (
	"net/http"
	"testing"
)

// goose, in the shape it actually writes: a version, whether it is applied, and
// when it ran.
const gooseDDL = `CREATE TABLE goose_db_version (
	id INTEGER PRIMARY KEY,
	version_id INTEGER NOT NULL,
	is_applied INTEGER NOT NULL,
	tstamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP
)`

const gooseRows = `INSERT INTO goose_db_version (version_id, is_applied, tstamp) VALUES
	(20240101120000, 1, '2024-01-01 12:00:00'),
	(20240202090000, 1, '2024-02-02 09:00:00'),
	(20240303080000, 0, '2024-03-03 08:00:00')`

// A database keeps no DDL history, so with no migration tool there is nothing
// to read — and that is an empty list, not an error.
func TestHistoryIsEmptyWithoutAMigrationTool(t *testing.T) {
	h := newServer(t, usersDDL)

	code, body := do(t, h, "GET", "/api/history", nil)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, body)
	}
	if entries, _ := body["entries"].([]any); len(entries) != 0 {
		t.Fatalf("want nothing, got %v", entries)
	}
}

func TestHistoryReadsAMigrationTable(t *testing.T) {
	h := newServer(t, usersDDL, gooseDDL, gooseRows)

	code, body := do(t, h, "GET", "/api/history", nil)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, body)
	}
	entries, _ := body["entries"].([]any)
	if len(entries) != 3 {
		t.Fatalf("want three entries, got %d: %v", len(entries), entries)
	}

	// Newest first: the question is nearly always about the last thing that
	// happened.
	first, _ := entries[0].(map[string]any)
	if first["source"] != "goose" {
		t.Errorf("want the goose entry, got %v", first["source"])
	}
	if first["version"] != "20240303080000" {
		t.Errorf("want the newest version first, got %v", first["version"])
	}
	// A row the tool marked unapplied must not be shown as applied.
	if first["applied"] != false {
		t.Errorf("want applied=false for the rolled-back entry, got %v", first["applied"])
	}
	if first["applied_at"] == nil {
		t.Error("goose records a timestamp; it should survive the round trip")
	}
}

// A table whose name matches but whose columns do not is skipped rather than
// failing the whole request: this is a display feature, and somebody else's
// table is not ours to be strict about.
func TestHistorySkipsATableItDoesNotRecognise(t *testing.T) {
	h := newServer(t, usersDDL,
		`CREATE TABLE flyway_schema_history (nothing_we_know_about TEXT)`)

	code, body := do(t, h, "GET", "/api/history", nil)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, body)
	}
	if entries, _ := body["entries"].([]any); len(entries) != 0 {
		t.Fatalf("want nothing, got %v", entries)
	}
}

// A change applied from the diagram appears in the history with its SQL. No
// migration tool will ever record it, so this log is the only trace it left.
func TestHistoryIncludesChangesAppliedHere(t *testing.T) {
	h := newServer(t, usersDDL)

	code, body := do(t, h, "POST", "/api/schema/apply", map[string]any{
		"changes": []map[string]any{{
			"kind": "add_column", "table": "users",
			"columns": []map[string]any{{"name": "nick", "type": "TEXT", "nullable": true}},
		}},
	})
	if code != http.StatusOK {
		t.Fatalf("apply: want 200, got %d: %v", code, body)
	}

	code, body = do(t, h, "GET", "/api/history", nil)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, body)
	}

	entries, _ := body["entries"].([]any)
	var found map[string]any
	for _, e := range entries {
		row, _ := e.(map[string]any)
		if row["source"] == "seedora" {
			found = row
			break
		}
	}
	if found == nil {
		t.Fatalf("the applied change is missing from the history: %v", entries)
	}
	if found["name"] != "added 1 column · users" {
		t.Errorf("summary: got %v", found["name"])
	}
	stmts, _ := found["statements"].([]any)
	if len(stmts) != 1 {
		t.Fatalf("want the SQL that ran, got %v", found["statements"])
	}
	if sql, _ := stmts[0].(string); sql != `ALTER TABLE "users" ADD COLUMN "nick" TEXT` {
		t.Errorf("statement: got %q", sql)
	}
}

func TestHistoryNeedsAConnection(t *testing.T) {
	h := disconnected(t)

	if code, _ := do(t, h, "GET", "/api/history", nil); code != http.StatusConflict {
		t.Fatalf("want 409, got %d", code)
	}
}
