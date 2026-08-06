package spec

import (
	"os"
	"strings"
	"testing"
)

// The README's configuration example is the first thing anyone copies. A
// documented format that does not parse is worse than none, so the example is
// extracted from the file itself and run through the real loader.
func TestREADMEExampleParses(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Skipf("README not readable: %v", err)
	}
	yaml := firstYAMLBlock(string(b))
	if yaml == "" {
		t.Fatal("no yaml block found in README.md")
	}

	p, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("the README example does not parse: %v\n\n%s", err, yaml)
	}
	if len(p.Tables) != 2 {
		t.Fatalf("parsed %d tables, want 2", len(p.Tables))
	}
	users := p.Tables["users"]
	if users == nil || users.Rows != 10000 {
		t.Fatalf("users did not parse as documented: %+v", users)
	}
	if e := users.Columns["email"]; e == nil || e.Generator != "email" || !e.Unique {
		t.Errorf("email column did not parse as documented: %+v", e)
	}
	if a := users.Columns["is_active"]; a == nil || a.TrueWeight == nil || *a.TrueWeight != 0.85 {
		t.Errorf("true_weight did not parse as documented: %+v", a)
	}
	fk := p.Tables["orders"].Columns["user_id"]
	if fk == nil || fk.References != "users.id" {
		t.Errorf("foreign key did not parse as documented: %+v", fk)
	}
}

func firstYAMLBlock(md string) string {
	const open = "```yaml\n"
	i := strings.Index(md, open)
	if i < 0 {
		return ""
	}
	rest := md[i+len(open):]
	j := strings.Index(rest, "```")
	if j < 0 {
		return ""
	}
	return rest[:j]
}
