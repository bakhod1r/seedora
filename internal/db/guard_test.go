package db

import "testing"

func TestGuardStopsProductionAndAllowsLocal(t *testing.T) {
	cases := []struct {
		dsn     string
		blocked bool
	}{
		// Local targets are the normal case and must never be guarded, or
		// people learn to pass the bypass flag by reflex.
		{"postgres://u:p@localhost:5432/myapp_dev", false},
		{"postgres://u:p@127.0.0.1:5432/myapp_prod", false},
		{"postgres://u:p@host.docker.internal:5432/app", false},
		{"postgres://u:p@192.168.1.10:5432/app", false},
		{"./dev.db", false},

		// Remote targets that name themselves.
		{"postgres://u:p@db.example.com:5432/myapp_prod", true},
		{"postgres://u:p@db.example.com:5432/production", true},
		{"mysql://u:p@shop.example.com:3306/live", true},

		// Managed hosts, where the database name is often innocuous and the
		// hostname is the only signal.
		{"postgres://u:p@x.eu-west-1.rds.amazonaws.com:5432/app", true},
		{"postgres://u:p@ep-cool-1.neon.tech/app", true},

		// Any non-local host at all.
		{"postgres://u:p@10.0.0.5:5432/scratch", false},
		{"postgres://u:p@staging.example.com:5432/scratch", true},
	}

	for _, c := range cases {
		err := Guard(c.dsn, false)
		if c.blocked && err == nil {
			t.Errorf("Guard(%q) allowed a production-looking target", c.dsn)
		}
		if !c.blocked && err != nil {
			t.Errorf("Guard(%q) blocked a development target: %v", c.dsn, err)
		}
		if err := Guard(c.dsn, true); err != nil {
			t.Errorf("Guard(%q) with the bypass still refused: %v", c.dsn, err)
		}
	}
}

func TestWordMatchDoesNotFireOnOrdinaryNames(t *testing.T) {
	// "product_catalog" contains "prod" and is an ordinary table name; matching
	// it would make the guard noise people learn to ignore.
	for _, name := range []string{"product_catalog", "reproduction", "prodigy"} {
		if wordMatch(name, "prod") {
			t.Errorf("wordMatch(%q, prod) fired on an ordinary name", name)
		}
	}
	for _, name := range []string{"prod", "myapp_prod", "prod-db", "app.prod"} {
		if !wordMatch(name, "prod") {
			t.Errorf("wordMatch(%q, prod) missed a real match", name)
		}
	}
}

func TestSchemeReadsBarePathsAsSQLite(t *testing.T) {
	cases := map[string]string{
		"postgres://u@h/db":  "postgres",
		"postgresql://u@h/d": "postgresql",
		"mysql://u@h/db":     "mysql",
		"file:./dev.db":      "sqlite",
		"./dev.db":           "sqlite",
		"/tmp/x.sqlite":      "sqlite",
	}
	for dsn, want := range cases {
		got, err := Scheme(dsn)
		if err != nil {
			t.Errorf("Scheme(%q): %v", dsn, err)
			continue
		}
		if got != want {
			t.Errorf("Scheme(%q) = %q, want %q", dsn, got, want)
		}
	}
	if _, err := Scheme(""); err == nil {
		t.Error("an empty DSN should be an error")
	}
}

// MySQL's own client library spells a DSN with no scheme at all, and people
// paste that in. Reading it as a SQLite filename — which is what any other
// schemeless string means — would fail with a nonsense error.
func TestSchemeReadsANativeMySQLDSN(t *testing.T) {
	for _, dsn := range []string{
		"root:secret@tcp(127.0.0.1:3306)/shop_dev",
		"root@unix(/tmp/mysql.sock)/shop_dev",
	} {
		got, err := Scheme(dsn)
		if err != nil {
			t.Fatal(err)
		}
		if got != "mysql" {
			t.Errorf("Scheme(%q) = %q, want mysql", dsn, got)
		}
	}

	tg := Describe("root:secret@tcp(db.example.com:3306)/shop_prod")
	if tg.Host != "db.example.com" || tg.Database != "shop_prod" {
		t.Errorf("Describe = %+v, want host db.example.com and database shop_prod", tg)
	}
	// And the guard has to see it, which is the reason Describe handles the
	// form at all.
	if err := Guard("root:secret@tcp(db.example.com:3306)/shop_prod", false); err == nil {
		t.Error("a production-looking native MySQL DSN was not guarded")
	}
}
