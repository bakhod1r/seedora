package store

import (
	"os"
	"strings"
	"testing"

	"github.com/bakhod1r/seedora/internal/config"
)

const nativeDSN = "alice:hun@ter2@tcp(db.internal:3306)/shop?parseTime=true"

// MySQL's own DSN has no scheme. It carries a password all the same, and
// keep_password=false has to mean the same thing for it.
func TestNativeMySQLPasswordIsNotStored(t *testing.T) {
	isolate(t)
	f, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Remember("", nativeDSN, false); err != nil {
		t.Fatal(err)
	}
	path, _ := Path()
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "hun@ter2") || strings.Contains(string(raw), "ter2") {
		t.Fatalf("password was written to disk:\n%s", raw)
	}
	c := f.Connections[0]
	if !c.HasPassword {
		t.Error("entry should record that a password is needed")
	}
	if c.Name != "shop on db.internal" {
		t.Errorf("name = %q, want %q", c.Name, "shop on db.internal")
	}
	back, err := WithPassword(c.DSN, "hun@ter2")
	if err != nil {
		t.Fatal(err)
	}
	if back != nativeDSN {
		t.Errorf("WithPassword = %q, want %q", back, nativeDSN)
	}
}

func TestQueryPasswordIsNotStored(t *testing.T) {
	isolate(t)
	f, _ := Load()
	if err := f.Remember("", "postgres://alice@db:5432/app?sslmode=require&password=s3cr3t", false); err != nil {
		t.Fatal(err)
	}
	path, _ := Path()
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "s3cr3t") {
		t.Fatalf("query password was written to disk:\n%s", raw)
	}
	if !f.Connections[0].HasPassword || !strings.Contains(f.Connections[0].DSN, "sslmode=require") {
		t.Errorf("entry = %+v", f.Connections[0])
	}
}

func TestRedactedMasksNativeMySQL(t *testing.T) {
	got := config.Redacted(nativeDSN)
	if strings.Contains(got, "ter2") {
		t.Fatalf("Redacted = %q leaks the password", got)
	}
	if !strings.HasPrefix(got, "alice:****@tcp(db.internal:3306)/shop") {
		t.Fatalf("Redacted = %q", got)
	}
}
