package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chdir moves into a scratch directory so the .env cascade reads the test's
// files and not whatever happens to sit in the repository.
func chdir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	return dir
}

func writeEnv(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A password is not shell syntax. Expansion would read the `$` in these as the
// start of a variable and silently hand the driver a different credential —
// `$ecret123` expands to nothing, `pa$word` truncates to `pa` — so the
// connection fails with an authentication error that points nowhere near the
// cause. This is the test that keeps expansion off.
func TestPasswordsWithDollarsSurviveIntact(t *testing.T) {
	for _, dsn := range []string{
		"postgres://u:pa$$word@localhost:5432/dev",
		"postgres://u:pa$word@localhost:5432/dev",
		"postgres://u:$ecret123@localhost:5432/dev",
		"postgres://u:${PATH}@localhost:5432/dev",
		"postgres://u:S3cure!x#y@localhost:5432/dev",
	} {
		dir := chdir(t)
		writeEnv(t, dir, "SEEDORA_DSN="+dsn+"\n")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("%s: %v", dsn, err)
		}
		if got := cfg.DSN.Value(); got != dsn {
			t.Errorf("DSN was rewritten:\n  in  %q\n  out %q", dsn, got)
		}
	}
}

// Seedora prints its target on startup and serves state over HTTP. A DSN that
// can reach either in cleartext is a leak, so the Secret wrapper is checked
// against every way a value normally escapes.
func TestDSNCannotLeakThroughLoggingOrJSON(t *testing.T) {
	dir := chdir(t)
	writeEnv(t, dir, "SEEDORA_DSN=postgres://alice:hunter2@db.internal:5432/app\n")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DSN.Value() == "" {
		t.Fatal("DSN did not load")
	}

	for _, rendered := range []string{
		fmt.Sprint(cfg.DSN),
		fmt.Sprintf("%v", cfg.DSN),
		fmt.Sprintf("%s", cfg.DSN),
		fmt.Sprintf("%+v", cfg),
		fmt.Sprintf("%#v", cfg),
	} {
		if strings.Contains(rendered, "hunter2") {
			t.Errorf("password reached a formatted string: %s", rendered)
		}
	}

	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "hunter2") {
		t.Errorf("password reached JSON: %s", b)
	}
}

func TestRedacted(t *testing.T) {
	cases := map[string]string{
		"postgres://alice:hunter2@db.internal:5432/app": "postgres://alice:****@db.internal:5432/app",
		"postgres://alice@db.internal:5432/app":         "postgres://alice@db.internal:5432/app",
		// A query string can carry a password of its own, so it is dropped
		// wholesale rather than parsed for the one parameter we know about.
		"postgres://u:p@h:5432/db?sslmode=disable&password=x": "postgres://u:****@h:5432/db",
		"./dev.db": "./dev.db",
	}
	for in, want := range cases {
		if got := Redacted(in); got != want {
			t.Errorf("Redacted(%q) = %q, want %q", in, got, want)
		}
	}
	// A string that does not parse must never be echoed back whole: the DSN
	// most likely to be unparseable is one with a stray character in its
	// password.
	if got := Redacted("postgres://u:p ss@h/db\n"); strings.Contains(got, "ss") {
		t.Errorf("an unparseable DSN was echoed: %q", got)
	}
}

func TestFlagBeatsEnvironment(t *testing.T) {
	dir := chdir(t)
	writeEnv(t, dir, "SEEDORA_DSN=postgres://from-file@h/db\n")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Resolve("postgres://from-flag@h/db"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.DSN.Value(); got != "postgres://from-flag@h/db" {
		t.Errorf("DSN = %q, want the flag to win", got)
	}
}

// SEEDORA_DSN_FILE is the Docker and Kubernetes path: the DSN is mounted at
// /run/secrets/... rather than passed in the environment.
func TestDSNFromMountedSecret(t *testing.T) {
	dir := chdir(t)
	secret := filepath.Join(dir, "dsn")
	// Secret managers append a newline; a DSN carrying one fails to connect
	// with an error that names neither the newline nor the file.
	if err := os.WriteFile(secret, []byte("postgres://u:p@h:5432/db\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeEnv(t, dir, "SEEDORA_DSN_FILE="+secret+"\n")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Resolve(""); err != nil {
		t.Fatal(err)
	}
	if got := cfg.DSN.Value(); got != "postgres://u:p@h:5432/db" {
		t.Errorf("DSN = %q, want the trailing newline stripped", got)
	}
}

func TestNoDSNIsReportedNotGuessed(t *testing.T) {
	chdir(t)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Resolve(""); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Connection(); err == nil {
		t.Error("a missing DSN should be reported, not defaulted to something")
	}
}

func TestDefaultsApply(t *testing.T) {
	chdir(t)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 7777 {
		t.Errorf("Port = %d, want 7777", cfg.Port)
	}
	if cfg.ConfigPath != "seedora.yaml" {
		t.Errorf("ConfigPath = %q, want seedora.yaml", cfg.ConfigPath)
	}
	if cfg.Locale != "en_US" {
		t.Errorf("Locale = %q, want en_US", cfg.Locale)
	}
	if cfg.Batch != 5000 {
		t.Errorf("Batch = %d, want 5000", cfg.Batch)
	}
}
