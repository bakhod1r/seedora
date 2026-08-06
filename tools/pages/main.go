// Command pages builds the static demo published to GitHub Pages.
//
// The demo is the real UI — the same HTML, stylesheet, and script the binary
// serves — with the server replaced by a file of recorded answers. That is the
// only honest way to demo a tool whose whole surface is a live database: a
// screenshot shows what it looks like, and a mock that behaves differently from
// the real thing teaches people something false.
//
// So the recording is taken from an actual run: this program opens the example
// schema in SQLite, introspects it, infers the plan, and writes exactly what
// the API would have returned.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	"github.com/bakhod1r/seedora/internal/db"
	_ "github.com/bakhod1r/seedora/internal/db/sqlite"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
	"github.com/bakhod1r/seedora/internal/plan"
	"github.com/bakhod1r/seedora/internal/seed"
	"github.com/bakhod1r/seedora/internal/ui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pages:", err)
		os.Exit(1)
	}
}

func run() error {
	schemaFile := flagOr(1, "examples/ecommerce/schema.sql")
	outDir := flagOr(2, "docs")

	sqlText, err := os.ReadFile(schemaFile)
	if err != nil {
		return err
	}

	// A file rather than :memory:, because the driver opens its own connection
	// and an in-memory database would not be the same one.
	tmp, err := os.MkdirTemp("", "seedora-pages-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	path := filepath.Join(tmp, "demo.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	if _, err := raw.Exec(string(sqlText)); err != nil {
		raw.Close()
		return fmt.Errorf("build the demo schema: %w", err)
	}
	raw.Close()

	ctx := context.Background()
	d, err := db.Open(ctx, path)
	if err != nil {
		return err
	}
	defer d.Close(ctx)

	schema, err := d.Introspect(ctx)
	if err != nil {
		return err
	}
	p := plan.Infer(schema)

	// The state payload, shaped exactly as /api/state returns it. The target is
	// a made-up name: nothing about the machine that built this belongs in a
	// file served to the public.
	state := map[string]any{
		"connected":   true,
		"engine":      "SQLite",
		"target":      "./shop.db",
		"schema":      schema,
		"plan":        p,
		"config_path": "seedora.yaml",
		"loaded":      false,
		"running":     false,
		"dialect":     ddl.SQLite,
		"types":       ddl.Types(ddl.SQLite),
	}
	if problems := p.Validate(schema); len(problems) > 0 {
		msgs := make([]string, 0, len(problems))
		for _, err := range problems {
			msgs = append(msgs, err.Error())
		}
		state["problems"] = msgs
	}

	// Previews are recorded per table, because generating them in the browser
	// would mean shipping the generators to it.
	previews := map[string]any{}
	for _, t := range schema.Tables {
		rows, cols, err := seed.Preview(ctx, d, schema, p, t.Name, 10, "en_US", 0)
		if err != nil {
			// A table that cannot be previewed is not a reason to fail the
			// build; the demo shows the error the real UI would show.
			previews[t.Name] = map[string]any{"error": err.Error()}
			continue
		}
		previews[t.Name] = map[string]any{"columns": cols, "rows": rows}
	}

	files := map[string]any{
		"state.json":      state,
		"generators.json": ui.Generators(),
		"previews.json":   previews,
		"history.json":    map[string]any{"entries": []model.Migration{}},
	}

	demoDir := filepath.Join(outDir, "demo")
	if err := os.MkdirAll(demoDir, 0o755); err != nil {
		return err
	}
	for name, body := range files {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(demoDir, name), b, 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%d bytes)\n", filepath.Join(demoDir, name), len(b))
	}

	// The UI's own files, copied rather than rewritten, so the demo cannot
	// drift from the product.
	for _, name := range []string{"app.css", "app.js"} {
		b, err := os.ReadFile(filepath.Join("internal/ui/assets", name))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(outDir, name), b, 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%d bytes)\n", filepath.Join(outDir, name), len(b))
	}

	// index.html gets one line added: the shim that answers the API, loaded
	// before the script that calls it.
	page, err := os.ReadFile("internal/ui/assets/index.html")
	if err != nil {
		return err
	}
	patched := insertBefore(string(page), `<script src="app.js"></script>`,
		"<script src=\"demo.js\"></script>\n")
	if err := os.WriteFile(filepath.Join(outDir, "index.html"), []byte(patched), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", filepath.Join(outDir, "index.html"))
	return nil
}

func insertBefore(s, marker, insert string) string {
	at := indexOf(s, marker)
	if at < 0 {
		return s
	}
	return s[:at] + insert + s[at:]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func flagOr(n int, fallback string) string {
	if len(os.Args) > n {
		return os.Args[n]
	}
	return fallback
}
