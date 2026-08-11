package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/bakhod1r/seedora/internal/config"
	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
	"github.com/bakhod1r/seedora/internal/plan"
	"github.com/bakhod1r/seedora/internal/seed"
	"github.com/bakhod1r/seedora/internal/spec"
	"github.com/bakhod1r/seedora/internal/ui"
)

// cmdUI starts the mapping editor. A DSN is optional: without one the page opens
// on its connect screen, which is the path someone who just installed Seedora
// takes.
func cmdUI(ctx context.Context, args []string) error {
	f, err := parseFlags("seedora", args)
	if err != nil {
		return err
	}
	cfg, err := f.load()
	if err != nil {
		return err
	}

	var (
		driver db.Driver
		dsn    string
		schema *model.Schema
		p      *plan.Plan
		loaded bool
	)
	if _, err := cfg.Connection(); err == nil {
		d, resolved, err := connect(ctx, cfg)
		if err != nil {
			return err
		}
		defer d.Close(context.WithoutCancel(ctx))
		s, err := d.Introspect(ctx)
		if err != nil {
			return err
		}
		p, loaded, err = spec.LoadOrInfer(cfg.ConfigPath, s)
		if err != nil {
			return err
		}
		driver, dsn, schema = d, resolved, s
	}

	srv := ui.New(cfg, driver, dsn, schema, p, loaded)

	ln, err := net.Listen("tcp", cfg.Addr())
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Addr(), err)
	}
	url := "http://" + ln.Addr().String()

	httpSrv := &http.Server{
		Handler: srv.Handler(),
		// The UI is a local page held open for as long as someone is editing;
		// a read timeout would be the only thing that could end that session.
		ReadHeaderTimeout: 10 * time.Second,
	}

	fmt.Printf("Seedora is at %s\n", url)
	if driver != nil {
		fmt.Printf("Connected to %s · %s\n", driver.Name(), config.Redacted(dsn))
	} else {
		fmt.Println("No DSN given — enter one in the browser.")
	}
	open(url)

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
		_ = srv.Close(shutdown)
		fmt.Println("\nStopped.")
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// cmdRun executes a saved config with no UI. This is the CI path.
func cmdRun(ctx context.Context, args []string) error {
	f, err := parseFlags("seedora run", args)
	if err != nil {
		return err
	}
	cfg, err := f.load()
	if err != nil {
		return err
	}

	d, dsn, err := connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer d.Close(context.WithoutCancel(ctx))

	s, err := d.Introspect(ctx)
	if err != nil {
		return err
	}
	// A branch that adds tables leaves the database behind the repository, and
	// there is nothing to seed in a table that does not exist yet. Creating the
	// missing ones first is what makes `seedora run` work on a fresh checkout.
	if cfg.Migrations != "" {
		s, err = applyMigrations(ctx, d, s, cfg.Migrations, f.quiet)
		if err != nil {
			return err
		}
	}
	p, loaded, err := spec.LoadOrInfer(cfg.ConfigPath, s)
	if err != nil {
		return err
	}
	if !loaded {
		fmt.Fprintf(os.Stderr,
			"no %s — seeding from inferred generators. Run `seedora scan` to write one.\n",
			cfg.ConfigPath)
	}

	fmt.Printf("Seeding %s · %s\n", d.Name(), config.Redacted(dsn))

	res, err := seed.Run(ctx, d, s, p, seed.Options{
		Seed:     cfg.Seed,
		Locale:   cfg.Locale,
		Rows:     cfg.Rows,
		Batch:    cfg.Batch,
		Truncate: cfg.Truncate,
		Append:   cfg.Append,
		DryRun:   cfg.DryRun,
		Progress: progressPrinter(f.quiet),
	})
	if err != nil {
		return err
	}
	report(res)
	return nil
}

// applyMigrations reads a project's migration files and creates the tables the
// database does not have, then re-introspects so everything downstream sees the
// database as it now is.
//
// It is not a migration runner: it records nothing, runs no down migrations,
// and touches no table that already exists. What it does is close the gap
// between a schema that lives in the repository and a development database
// that has not caught up with it.
func applyMigrations(ctx context.Context, d db.Driver, s *model.Schema, path string, quiet bool) (*model.Schema, error) {
	changes, files, err := ddl.Scan(path)
	if err != nil {
		return nil, err
	}
	missing, existing := ddl.Missing(s, changes)
	if !quiet {
		fmt.Printf("Read %d migration file(s) from %s · %d table(s), %d already in the database\n",
			len(files), path, len(changes), len(existing))
	}
	if len(missing) == 0 {
		return s, nil
	}

	stmts, err := ddl.Plan(d.Dialect(), s, missing)
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	for _, stmt := range stmts {
		if err := tx.Exec(ctx, stmt); err != nil {
			return nil, fmt.Errorf("%s: %w", firstLine(stmt), err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	if !quiet {
		names := make([]string, 0, len(missing))
		for _, c := range missing {
			names = append(names, c.Table)
		}
		fmt.Printf("Created %d table(s): %s\n", len(names), strings.Join(names, ", "))
	}
	return d.Introspect(ctx)
}

// firstLine keeps an error about a failed statement to one line: the statement
// is a formatted CREATE TABLE and the whole of it in an error message buries
// what the database said.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

// cmdScan introspects a schema and writes a starter config. Re-running it over
// an existing file keeps every override and adds only what is new.
func cmdScan(ctx context.Context, args []string) error {
	f, err := parseFlags("seedora scan", args)
	if err != nil {
		return err
	}
	cfg, err := f.load()
	if err != nil {
		return err
	}
	out := f.out
	if out == "" {
		out = cfg.ConfigPath
	}

	d, _, err := connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer d.Close(context.WithoutCancel(ctx))

	s, err := d.Introspect(ctx)
	if err != nil {
		return err
	}
	p, loaded, err := spec.LoadOrInfer(out, s)
	if err != nil {
		return err
	}
	if err := spec.Save(out, p, s); err != nil {
		return err
	}

	tables, columns, unsure := 0, 0, 0
	for _, tp := range p.Tables {
		tables++
		for _, cp := range tp.Columns {
			columns++
			if cp.Confidence == plan.Low {
				unsure++
			}
		}
	}
	verb := "Wrote"
	if loaded {
		verb = "Updated"
	}
	fmt.Printf("%s %s · %d tables, %d columns\n", verb, out, tables, columns)
	if unsure > 0 {
		fmt.Printf("%d columns could not be inferred confidently — run `seedora` to review them.\n", unsure)
	}
	return nil
}

// cmdValidate checks a config against the live schema and exits non-zero if it
// does not fit, which is what makes it useful in a pre-commit hook or CI.
func cmdValidate(ctx context.Context, args []string) error {
	f, err := parseFlags("seedora validate", args)
	if err != nil {
		return err
	}
	cfg, err := f.load()
	if err != nil {
		return err
	}

	p, err := spec.Load(cfg.ConfigPath)
	if err != nil {
		return err
	}

	d, _, err := connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer d.Close(context.WithoutCancel(ctx))

	s, err := d.Introspect(ctx)
	if err != nil {
		return err
	}

	problems := p.Validate(s)
	if len(problems) == 0 {
		fmt.Printf("%s is valid against %s\n", cfg.ConfigPath, d.Name())
		return nil
	}
	for _, e := range problems {
		fmt.Fprintln(os.Stderr, "  "+e.Error())
	}
	return fmt.Errorf("%d problems in %s", len(problems), cfg.ConfigPath)
}

// progressPrinter writes a single rewriting line, so a long run shows movement
// without scrolling a terminal full of it.
func progressPrinter(quiet bool) func(seed.Progress) {
	if quiet {
		return nil
	}
	var last time.Time
	return func(p seed.Progress) {
		// Throttled: the callback fires per batch, and a terminal repaint per
		// batch is slower than the work it reports on.
		if time.Since(last) < 100*time.Millisecond && p.Written < p.Total {
			return
		}
		last = time.Now()
		pct := 0
		if p.Total > 0 {
			pct = p.Written * 100 / p.Total
		}
		fmt.Printf("\r  [%d/%d] %-24s %s / %s (%d%%)   ",
			p.TableIndex+1, p.TableCount, p.Table,
			human(int64(p.Written)), human(int64(p.Total)), pct)
		if p.Written >= p.Total {
			fmt.Println()
		}
	}
}

func report(r *seed.Result) {
	verb := "Seeded"
	if r.DryRun {
		verb = "Validated"
	}
	fmt.Printf("\n%s %s rows in %s", verb, human(r.Rows), r.Duration.Round(time.Millisecond))
	if rate := r.RowsPerSecond(); rate > 0 {
		fmt.Printf(" · %s rows/s", human(int64(rate)))
	}
	fmt.Printf("\nSeed %d — pass --seed %d to reproduce this exactly.\n", r.Seed, r.Seed)
	if r.DryRun {
		fmt.Println("Nothing was written.")
	}
}

// human formats a count with thousands separators.
func human(n int64) string {
	s := fmt.Sprint(n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// open launches the default browser. A failure is not an error: the URL is
// already on stdout, and a headless machine has no browser to launch.
func open(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
