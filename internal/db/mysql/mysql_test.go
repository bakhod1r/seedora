package mysql

import (
	"strings"
	"testing"
)

// A URL DSN is what people type, because it is what every other tool takes.
// The driver wants its own form, and both have to arrive at the same server.
func TestNativeDSNAcceptsBothForms(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "url",
			in:   "mysql://root:secret@localhost:3306/shop_dev",
			want: []string{"root:secret@tcp(localhost:3306)/shop_dev", "parseTime=true"},
		},
		{
			name: "url without a port",
			in:   "mysql://root@db.internal/shop",
			want: []string{"tcp(db.internal:3306)/shop"},
		},
		{
			name: "mariadb scheme",
			in:   "mariadb://root:secret@127.0.0.1:3307/app",
			want: []string{"tcp(127.0.0.1:3307)/app"},
		},
		{
			name: "unix socket",
			in:   "mysql://root@/shop?socket=/tmp/mysql.sock",
			want: []string{"unix(/tmp/mysql.sock)/shop"},
		},
		{
			name: "already native",
			in:   "root:secret@tcp(127.0.0.1:3306)/shop",
			want: []string{"tcp(127.0.0.1:3306)/shop", "parseTime=true"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NativeDSN(c.in)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range c.want {
				if !strings.Contains(got, want) {
					t.Errorf("NativeDSN(%q) = %q, want it to contain %q", c.in, got, want)
				}
			}
		})
	}
}

// A password is not shell syntax and not URL syntax either: whatever the user
// typed has to reach the server unchanged.
func TestNativeDSNKeepsAnAwkwardPassword(t *testing.T) {
	got, err := NativeDSN("mysql://root:p%40ss%3Aword@localhost:3306/app")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "root:p@ss:word@tcp(") {
		t.Errorf("password did not survive: %q", got)
	}
}

// MySQL declares an enum inline rather than as a named type, so the labels are
// only ever available as text out of the catalog.
func TestEnumLabels(t *testing.T) {
	cases := map[string][]string{
		"enum('draft','sent','paid')": {"draft", "sent", "paid"},
		"enum('a')":                   {"a"},
		// A quote inside a label is doubled, and undoing that is the only escape
		// the format has.
		"enum('it''s','ok')": {"it's", "ok"},
		"varchar(20)":        nil,
	}

	for in, want := range cases {
		got := enumLabels(in)
		if len(got) != len(want) {
			t.Errorf("enumLabels(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("enumLabels(%q)[%d] = %q, want %q", in, i, got[i], want[i])
			}
		}
	}
}

// The chunked INSERT is the whole fast path, and a placeholder list that is
// wrong by one is a syntax error at a hundred thousand rows.
func TestPlaceholders(t *testing.T) {
	if got, want := placeholders(2, 3), "(?,?,?),(?,?,?)"; got != want {
		t.Errorf("placeholders(2, 3) = %q, want %q", got, want)
	}
}
