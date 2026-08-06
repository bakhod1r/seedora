package ddl

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bakhod1r/seedora/internal/model"
)

// write lays out a migration directory the way a project has one: numbered
// files, some of them down migrations.
func write(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The files are replayed in order, so a table's shape is what the last
// migration left it as, not what the first one created.
func TestScanReplaysInOrder(t *testing.T) {
	dir := write(t, map[string]string{
		"0001_users.up.sql": `
CREATE TABLE users (
  id BIGINT PRIMARY KEY,
  email VARCHAR(120) NOT NULL UNIQUE
);`,
		"0002_orders.up.sql": `
CREATE TABLE orders (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES users(id)
);`,
		"0003_alter.up.sql": `
ALTER TABLE users ADD COLUMN city VARCHAR(60);
CREATE INDEX users_city_idx ON users (city);
ALTER TABLE users DROP COLUMN email;`,
		// Down migrations must not be replayed: this one drops both tables.
		"0003_alter.down.sql": `DROP TABLE orders; DROP TABLE users;`,
	})

	changes, files, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Errorf("read %d files, want 3", len(files))
	}
	if len(changes) != 2 {
		t.Fatalf("got %d tables, want users and orders", len(changes))
	}

	users := changes[0]
	if users.Table != "users" {
		t.Fatalf("first table is %s, want users", users.Table)
	}
	if hasColumn(users, "email") {
		t.Error("email was dropped by 0003 and is still here")
	}
	if !hasColumn(users, "city") {
		t.Error("city was added by 0003 and is missing")
	}
	// orders comes second because it was created second, and Plan is what
	// orders the statements for execution.
	if changes[1].Table != "orders" {
		t.Errorf("second table is %s, want orders", changes[1].Table)
	}
}

// A file with both directions in it — goose's format — must contribute only its
// up half.
func TestScanIgnoresTheDownHalfOfAFile(t *testing.T) {
	dir := write(t, map[string]string{
		"001_init.sql": `
-- +goose Up
CREATE TABLE items (id BIGINT PRIMARY KEY, name TEXT NOT NULL);

-- +goose Down
DROP TABLE items;`,
	})

	changes, _, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Table != "items" {
		t.Fatalf("got %v, want one table named items", changes)
	}
}

// A directory named for rollbacks is skipped whole.
func TestScanSkipsADownDirectory(t *testing.T) {
	dir := write(t, map[string]string{
		"up/001_users.sql":   `CREATE TABLE users (id BIGINT PRIMARY KEY);`,
		"down/001_users.sql": `DROP TABLE users;`,
	})

	changes, _, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Table != "users" {
		t.Fatalf("got %v, want one table named users", changes)
	}
}

// Missing is the whole point of scanning: what the repository has that the
// database does not.
func TestMissingSplitsAgainstTheLiveSchema(t *testing.T) {
	live := &model.Schema{Tables: []*model.Table{{Name: "users"}}}
	changes := []Change{
		{Kind: CreateTable, Table: "users"},
		{Kind: CreateTable, Table: "orders"},
	}

	missing, existing := Missing(live, changes)
	if len(missing) != 1 || missing[0].Table != "orders" {
		t.Errorf("missing = %v, want orders", missing)
	}
	if len(existing) != 1 || existing[0] != "users" {
		t.Errorf("existing = %v, want users", existing)
	}
}

// A directory with no SQL in it is a mistyped path, and saying so beats
// returning an empty schema that looks like an up-to-date database.
func TestScanReportsAnEmptyDirectory(t *testing.T) {
	if _, _, err := Scan(t.TempDir()); err == nil {
		t.Fatal("scanning an empty directory succeeded")
	}
}
