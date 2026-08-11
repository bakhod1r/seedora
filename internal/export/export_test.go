package export_test

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/bakhod1r/seedora/internal/db"
	_ "github.com/bakhod1r/seedora/internal/db/sqlite"
	"github.com/bakhod1r/seedora/internal/export"
	"github.com/bakhod1r/seedora/internal/model"
	"github.com/bakhod1r/seedora/internal/plan"
	"github.com/bakhod1r/seedora/internal/seed"
)

const schemaSQL = `
CREATE TABLE users (
  id         INTEGER PRIMARY KEY,
  email      VARCHAR(60) NOT NULL UNIQUE,
  first_name VARCHAR(50) NOT NULL,
  city       VARCHAR(60),
  is_active  BOOLEAN NOT NULL,
  created_at TIMESTAMP NOT NULL
);

CREATE TABLE orders (
  id      INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id),
  status  VARCHAR(20) NOT NULL,
  total   DECIMAL(10,2) NOT NULL
);
`

// open builds the database the schema is read from. Export never writes to it;
// several of the tests below check that it did not.
func open(t *testing.T) (db.Driver, *model.Schema, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(schemaSQL); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })

	ctx := t.Context()
	d, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close(context.Background()) })

	s, err := d.Introspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return d, s, raw
}

func run(t *testing.T, format export.Format, users, orders int) (string, *export.Driver, *sql.DB) {
	t.Helper()
	d, s, raw := open(t)
	dir := filepath.Join(t.TempDir(), "out")

	w, err := export.New(d, dir, format)
	if err != nil {
		t.Fatal(err)
	}
	p := plan.Infer(s)
	p.Tables["users"].Rows = users
	p.Tables["orders"].Rows = orders

	if _, err := seed.Run(t.Context(), w, s, p, seed.Options{Seed: 9, Batch: 32}); err != nil {
		t.Fatal(err)
	}
	return dir, w, raw
}

// The point of the feature: files with the right rows in them.
func TestCSVCarriesEveryRowAndAHeader(t *testing.T) {
	dir, _, _ := run(t, export.CSV, 120, 300)

	f, err := os.Open(filepath.Join(dir, "users.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 121 {
		t.Fatalf("users.csv has %d lines, want 121 (a header and 120 rows)", len(records))
	}
	header := records[0]
	if len(header) == 0 || header[0] == "" {
		t.Fatalf("no header: %v", header)
	}
	// Every row has to have the header's shape, or the file is not a CSV any
	// loader will take.
	for i, r := range records[1:] {
		if len(r) != len(header) {
			t.Fatalf("row %d has %d fields, header has %d", i, len(r), len(header))
		}
	}
}

func TestJSONIsAnArrayOfObjectsInColumnOrder(t *testing.T) {
	dir, _, _ := run(t, export.JSON, 40, 0)

	body, err := os.ReadFile(filepath.Join(dir, "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if len(rows) != 40 {
		t.Errorf("users.json holds %d rows, want 40", len(rows))
	}
	if _, ok := rows[0]["email"]; !ok {
		t.Errorf("row has no email: %v", rows[0])
	}

	// Column order, not alphabetical: email comes before first_name in the
	// table, and after it in an alphabetical sort of the same keys.
	text := string(body)
	if strings.Index(text, `"email"`) > strings.Index(text, `"is_active"`) {
		t.Error("keys are not in the table's column order")
	}
}

// The SQL file has to be replayable, which is the only claim that matters and
// the one worth checking by replaying it.
func TestSQLFileLoadsIntoAFreshDatabase(t *testing.T) {
	dir, _, _ := run(t, export.SQL, 50, 100)

	fresh := filepath.Join(t.TempDir(), "fresh.db")
	raw, err := sql.Open("sqlite", fresh)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(schemaSQL); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"users.sql", "orders.sql"} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec(string(body)); err != nil {
			t.Fatalf("%s did not replay: %v", name, err)
		}
	}

	var users, orders, orphans int
	if err := raw.QueryRow(`SELECT
		(SELECT COUNT(*) FROM users),
		(SELECT COUNT(*) FROM orders),
		(SELECT COUNT(*) FROM orders o LEFT JOIN users u ON u.id = o.user_id WHERE u.id IS NULL)`).
		Scan(&users, &orders, &orphans); err != nil {
		t.Fatal(err)
	}
	if users != 50 || orders != 100 {
		t.Errorf("replayed %d users and %d orders, want 50 and 100", users, orders)
	}
	// The reason ReadKeys answers from the run: a child exported next to its
	// parent must point at the parent rows in the fixture.
	if orphans != 0 {
		t.Errorf("%d orders point at no user after replay", orphans)
	}
}

// Export reads the schema from a database and must leave it exactly as it was.
func TestExportWritesNothingToTheDatabase(t *testing.T) {
	_, _, raw := run(t, export.CSV, 200, 400)

	var users, orders int
	if err := raw.QueryRow(
		`SELECT (SELECT COUNT(*) FROM users), (SELECT COUNT(*) FROM orders)`).
		Scan(&users, &orders); err != nil {
		t.Fatal(err)
	}
	if users != 0 || orders != 0 {
		t.Errorf("the database holds %d users and %d orders: export wrote to it", users, orders)
	}
}

// The files are listed in the order they were written, which is the order they
// have to be loaded back in — a child before its parent fails on the foreign
// key.
func TestFilesAreListedInLoadOrder(t *testing.T) {
	_, w, _ := run(t, export.CSV, 10, 10)

	files := w.Files()
	if len(files) != 2 {
		t.Fatalf("wrote %d files, want 2: %v", len(files), files)
	}
	if filepath.Base(files[0]) != "users.csv" {
		t.Errorf("parent is not first: %v", files)
	}
}

// The same seed produces the same file, which is what makes a fixture something
// that can be committed and diffed.
func TestTheSameSeedProducesTheSameFile(t *testing.T) {
	first, _, _ := run(t, export.CSV, 100, 0)
	second, _, _ := run(t, export.CSV, 100, 0)

	a, err := os.ReadFile(filepath.Join(first, "users.csv"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(second, "users.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Error("the same seed produced a different file")
	}
}

// A DECIMAL(10,2) column holds two decimal places. Writing the generator's
// full binary float instead produces a fixture that is not what seeding the
// same plan would have stored.
func TestDecimalsAreWrittenAtTheirDeclaredScale(t *testing.T) {
	dir, _, _ := run(t, export.CSV, 20, 40)

	f, err := os.Open(filepath.Join(dir, "orders.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	total := -1
	for i, name := range records[0] {
		if name == "total" {
			total = i
		}
	}
	if total < 0 {
		t.Fatalf("no total column: %v", records[0])
	}

	for _, r := range records[1:] {
		value := r[total]
		dot := strings.IndexByte(value, '.')
		if dot < 0 {
			continue
		}
		if places := len(value) - dot - 1; places > 2 {
			t.Fatalf("total %q has %d decimal places, the column declares 2", value, places)
		}
	}
}

func TestParseFormatRejectsWhatItCannotWrite(t *testing.T) {
	for _, name := range []string{"csv", "JSON", "sql"} {
		if _, err := export.ParseFormat(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := export.ParseFormat("parquet"); err == nil {
		t.Error("expected a refusal for an unsupported format")
	}
}

// A failed run must leave no partial fixture. Nothing is written before Commit
// precisely so there is nothing to clean up.
func TestAFailedRunWritesNoFiles(t *testing.T) {
	d, s, _ := open(t)
	dir := filepath.Join(t.TempDir(), "out")

	w, err := export.New(d, dir, export.CSV)
	if err != nil {
		t.Fatal(err)
	}
	p := plan.Infer(s)
	// More distinct emails than the generator can produce for a 60-character
	// column is refused before anything is written.
	p.Tables["users"].Rows = 100
	p.Tables["orders"].Rows = 0

	tx, err := w.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a rolled-back run left %d file(s) behind", len(entries))
	}
}

// Schema changes have no meaning in a directory of files, and quietly ignoring
// one would produce a fixture missing the column it was about to add.
func TestExportRefusesSchemaChanges(t *testing.T) {
	d, _, _ := open(t)
	w, err := export.New(d, filepath.Join(t.TempDir(), "out"), export.CSV)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := w.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())

	if err := tx.Exec(t.Context(), "CREATE TABLE x (id INTEGER)"); err == nil {
		t.Error("expected a refusal")
	}
}
