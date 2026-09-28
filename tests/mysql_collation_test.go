package tests

import (
	"os"
	"testing"
)

// A column under a case-insensitive collation is unique over its folded value:
// MySQL rejects "Bob" beside "bob". Introspection has to say so, or the seeder
// keeps both and the database refuses the second.
func TestMySQLCaseInsensitiveCollationFoldsUniqueness(t *testing.T) {
	dsn := os.Getenv("SEEDORA_TEST_MYSQL")
	if dsn == "" {
		t.Skip("no mysql")
	}
	tg := openMySQL(t, "mysql", "SEEDORA_TEST_MYSQL", dsn)
	if _, err := tg.raw.Exec("DROP TABLE IF EXISTS sx_handles; " +
		"CREATE TABLE sx_handles (id INT PRIMARY KEY, " +
		"ci VARCHAR(20) COLLATE utf8mb4_0900_ai_ci UNIQUE, " +
		"bin VARCHAR(20) COLLATE utf8mb4_bin UNIQUE)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = tg.raw.Exec("DROP TABLE IF EXISTS sx_handles") })

	s, err := tg.driver.Introspect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	tb := s.Table("sx_handles")
	if tb == nil {
		t.Fatal("sx_handles not introspected")
	}
	if !tb.Column("ci").UniqueFold {
		t.Error("ci: a _ci collation should fold uniqueness")
	}
	if tb.Column("bin").UniqueFold {
		t.Error("bin: a binary collation compares bytes and should not fold")
	}
}
