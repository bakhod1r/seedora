package store

import (
	"os"
	"strings"
	"testing"
)

// isolate points the store at a temporary config directory, so a test never
// reads or overwrites the developer's own connections.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
}

const dsn = "postgres://alice:hunter2@db.internal:5432/myapp_dev"

func TestPasswordIsNotStoredByDefault(t *testing.T) {
	isolate(t)

	f, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Remember("", dsn, false); err != nil {
		t.Fatal(err)
	}

	// The check that matters is on the bytes, not on the struct: a password
	// that reaches the disk has leaked whatever the API says.
	path, _ := Path()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hunter2") {
		t.Fatalf("password was written to disk:\n%s", raw)
	}

	back, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Connections) != 1 {
		t.Fatalf("stored %d connections, want 1", len(back.Connections))
	}
	c := back.Connections[0]
	if !c.HasPassword {
		t.Error("the entry should record that a password is needed on reconnect")
	}
	if strings.Contains(c.DSN, "hunter2") {
		t.Errorf("stored DSN still carries the password: %s", c.DSN)
	}
	if !strings.Contains(c.DSN, "alice") {
		t.Errorf("stored DSN lost the username: %s", c.DSN)
	}
	if c.Name != "myapp_dev on db.internal" {
		t.Errorf("name = %q, want %q", c.Name, "myapp_dev on db.internal")
	}
}

func TestPasswordIsStoredWhenAsked(t *testing.T) {
	isolate(t)

	f, _ := Load()
	if err := f.Remember("", dsn, true); err != nil {
		t.Fatal(err)
	}
	back, _ := Load()
	c := back.Connections[0]
	if !strings.Contains(c.DSN, "hunter2") {
		t.Error("the password should be stored when the user asked for it")
	}
	if c.HasPassword {
		t.Error("an entry that holds its password should not ask for one")
	}
}

func TestStoreFileIsOwnerOnly(t *testing.T) {
	isolate(t)

	f, _ := Load()
	if err := f.Remember("", dsn, true); err != nil {
		t.Fatal(err)
	}
	path, _ := Path()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("connections file mode = %o, want 600", perm)
	}
}

func TestRememberReplacesRatherThanDuplicates(t *testing.T) {
	isolate(t)

	f, _ := Load()
	// The same database, connected to twice with different passwords, is one
	// connection — not two entries differing in a field the list does not show.
	if err := f.Remember("", dsn, false); err != nil {
		t.Fatal(err)
	}
	if err := f.Remember("", "postgres://alice:corrected@db.internal:5432/myapp_dev", true); err != nil {
		t.Fatal(err)
	}
	back, _ := Load()
	if len(back.Connections) != 1 {
		t.Fatalf("stored %d connections, want 1", len(back.Connections))
	}
	if !strings.Contains(back.Connections[0].DSN, "corrected") {
		t.Error("the newer connection should have replaced the older one")
	}
}

func TestForget(t *testing.T) {
	isolate(t)

	f, _ := Load()
	if err := f.Remember("", dsn, false); err != nil {
		t.Fatal(err)
	}
	if err := f.Forget(dsn); err != nil {
		t.Fatal(err)
	}
	back, _ := Load()
	if len(back.Connections) != 0 {
		t.Errorf("forget left %d connections", len(back.Connections))
	}
}

func TestWithPassword(t *testing.T) {
	stripped, _ := stripPassword(dsn)
	full, err := WithPassword(stripped, "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if full != dsn {
		t.Errorf("WithPassword = %q, want %q", full, dsn)
	}

	// A SQLite path has nowhere to put a password, and guessing would produce a
	// connection string that fails somewhere less obvious.
	if _, err := WithPassword("./dev.db", "x"); err == nil {
		t.Error("adding a password to a bare path should be an error")
	}
}

func TestListIsCapped(t *testing.T) {
	isolate(t)

	f, _ := Load()
	for i := range maxConnections + 5 {
		d := "postgres://u@h:5432/db" + string(rune('a'+i))
		if err := f.Remember("", d, false); err != nil {
			t.Fatal(err)
		}
	}
	back, _ := Load()
	if len(back.Connections) != maxConnections {
		t.Errorf("stored %d connections, want the cap of %d", len(back.Connections), maxConnections)
	}
}

func TestMissingStoreIsNotAnError(t *testing.T) {
	isolate(t)

	f, err := Load()
	if err != nil {
		t.Fatalf("a first run with no store should not fail: %v", err)
	}
	if len(f.Connections) != 0 {
		t.Error("a fresh store should be empty")
	}
}

// The stored DSN is what a later run connects with, so stripping the password
// must not disturb anything else. url.Parse round-trips re-encode as they go,
// which is how a DSN quietly stops working between sessions.
func TestStrippingPasswordLeavesTheRestAlone(t *testing.T) {
	cases := map[string]string{
		"postgres://alice:hunter2@db.internal:5432/myapp_dev?sslmode=require": "postgres://alice@db.internal:5432/myapp_dev?sslmode=require",
		"postgres://alice@db.internal:5432/myapp_dev":                         "postgres://alice@db.internal:5432/myapp_dev",
		"mysql://root:pw@127.0.0.1:3306/app":                                  "mysql://root@127.0.0.1:3306/app",
		"./dev.db":                                                            "./dev.db",
	}
	for in, want := range cases {
		got, _ := stripPassword(in)
		if got != want {
			t.Errorf("stripPassword(%q)\n  = %q\n want %q", in, got, want)
		}
	}
}

// The masked form is what the connect screen shows. It must be readable, and it
// must still show a SQLite path — which has no password to hide.
func TestRedactedIsReadable(t *testing.T) {
	cases := map[string]string{
		"postgres://alice:hunter2@db.internal:5432/app": "postgres://alice:****@db.internal:5432/app",
		"./dev.db": "./dev.db",
	}
	for in, want := range cases {
		c := Connection{DSN: in}
		if got := c.Redacted(); got != want {
			t.Errorf("Redacted(%q) = %q, want %q", in, got, want)
		}
	}
}
