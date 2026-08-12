// Command seedora fills a database with realistic data.
//
// With no subcommand it starts the mapping UI. The subcommands are the headless
// paths: `run` executes a saved config, `scan` writes a starter one, `validate`
// checks one against a live schema.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/bakhod1r/seedora/internal/config"
	"github.com/bakhod1r/seedora/internal/db"

	// Drivers register themselves, so linking one in is what enables its DSN
	// scheme. Adding an engine is an import here and a package under internal/db.
	// The engines in the default build live in drivers_default.go; the rest are
	// behind build tags, one file per tag set. Which engines a binary has is
	// therefore a property of how it was built, and `seedora --help` lists what
	// this one actually carries.
	_ "github.com/bakhod1r/seedora/internal/db/mysql"
	_ "github.com/bakhod1r/seedora/internal/db/postgres"
	_ "github.com/bakhod1r/seedora/internal/db/sqlite"
)

// Build information, set by the linker in a release build.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	// A Ctrl-C during a run cancels the context, which rolls the transaction
	// back — the database is left exactly as it was, which is the whole promise.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "cancelled — nothing was written")
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "seedora: "+err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "run":
			return cmdRun(ctx, args[1:])
		case "dump":
			return cmdDump(ctx, args[1:])
		case "scan":
			return cmdScan(ctx, args[1:])
		case "validate":
			return cmdValidate(ctx, args[1:])
		case "version":
			fmt.Printf("seedora %s (%s, built %s)\n", version, commit, date)
			return nil
		case "help", "-h", "--help":
			usage(os.Stdout)
			return nil
		}
		if args[0][0] != '-' {
			usage(os.Stderr)
			return fmt.Errorf("unknown command %q", args[0])
		}
	}
	return cmdUI(ctx, args)
}

func usage(w *os.File) {
	fmt.Fprint(w, `seedora — schema-aware database seeding

Usage:
  seedora [flags]                    start the mapping UI
  seedora run      [flags]           run a saved config, no UI
  seedora dump     [flags]           generate the same rows into files, not the database
  seedora scan     [flags]           introspect a schema and write a starter config
  seedora validate [flags]           check a config against the live schema
  seedora version                    print version and build info

Common flags:
  --dsn <string>       connection string; falls back to SEEDORA_DSN
  --config <file>      mapping file (default seedora.yaml)
  --rows <n>           override the row count for every table
  --seed <n>           fix the random seed for reproducible output
  --truncate           truncate target tables before seeding
  --append             add rows to tables that already have some; nothing is
                       emptied, and unique columns are read back first
  --dry-run            generate and validate without writing
  --migrations <path>  migration directory or .sql file; tables in it that the
                       database lacks are created first
  --port <n>           UI port (default 7777)
  --host <addr>        UI bind address (default 127.0.0.1)
  --locale <name>      generator locale (default en_US)
  --batch <n>          rows generated per unit of work (default 5000)
  --quiet              suppress the progress line
  -o <path>            where to write: the config (scan) or the directory (dump)
  --format <name>      dump format: csv, json, or sql (default csv)
  --i-know-what-im-doing
                       bypass the production-target guard

Environment:
`)
	_ = config.Usage(w)
}

// flags is the shared flag set. Every command takes the same ones, because a
// flag that works in one and not another is a thing to remember rather than a
// thing to use.
type flags struct {
	fs *flag.FlagSet

	dsn      string
	cfgPath  string
	rows     int
	seed     uint64
	truncate bool
	appendTo bool
	dryRun   bool
	port     int
	host     string
	locale   string
	batch    int
	force    bool
	out      string
	format   string
	quiet    bool

	migrations string
}

func parseFlags(name string, args []string) (*flags, error) {
	f := &flags{fs: flag.NewFlagSet(name, flag.ContinueOnError)}
	f.fs.StringVar(&f.dsn, "dsn", "", "connection string")
	f.fs.StringVar(&f.cfgPath, "config", "", "mapping file")
	f.fs.StringVar(&f.out, "o", "", "output path: the config (scan) or the directory (dump)")
	f.fs.StringVar(&f.format, "format", "csv", "dump file format: csv, json, or sql")
	f.fs.IntVar(&f.rows, "rows", 0, "override the row count for every table")
	f.fs.Uint64Var(&f.seed, "seed", 0, "fix the random seed")
	f.fs.BoolVar(&f.truncate, "truncate", false, "truncate target tables first")
	f.fs.BoolVar(&f.appendTo, "append", false, "add rows to tables that already have some")
	f.fs.BoolVar(&f.dryRun, "dry-run", false, "generate and validate without writing")
	f.fs.IntVar(&f.port, "port", 0, "UI port")
	f.fs.StringVar(&f.host, "host", "", "UI bind address")
	f.fs.StringVar(&f.locale, "locale", "", "generator locale")
	f.fs.IntVar(&f.batch, "batch", 0, "rows generated per unit of work")
	f.fs.BoolVar(&f.force, "i-know-what-im-doing", false, "bypass the production-target guard")
	f.fs.BoolVar(&f.quiet, "quiet", false, "suppress progress output")
	f.fs.StringVar(&f.migrations, "migrations", "",
		"migration directory or .sql file: tables in it that the database lacks are created first")
	f.fs.Usage = func() { usage(os.Stderr) }
	if err := f.fs.Parse(args); err != nil {
		return nil, err
	}
	return f, nil
}

// load resolves configuration: environment and .env first, then flags on top.
func (f *flags) load() (*config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if err := cfg.Resolve(f.dsn); err != nil {
		return nil, err
	}
	if f.cfgPath != "" {
		cfg.ConfigPath = f.cfgPath
	}
	if f.port != 0 {
		cfg.Port = f.port
	}
	if f.host != "" {
		cfg.Host = f.host
	}
	if f.locale != "" {
		cfg.Locale = f.locale
	}
	if f.batch != 0 {
		cfg.Batch = f.batch
	}
	if f.rows != 0 {
		cfg.Rows = f.rows
	}
	if f.seed != 0 {
		cfg.Seed = f.seed
	}
	if f.truncate {
		cfg.Truncate = true
	}
	if f.appendTo {
		cfg.Append = true
	}
	if f.dryRun {
		cfg.DryRun = true
	}
	if f.force {
		cfg.AllowProduction = true
	}
	if f.migrations != "" {
		cfg.Migrations = f.migrations
	}
	return cfg, nil
}

// connect opens the database named by the config, after the guard.
func connect(ctx context.Context, cfg *config.Config) (db.Driver, string, error) {
	dsn, err := cfg.Connection()
	if err != nil {
		return nil, "", err
	}
	if err := db.Guard(dsn, cfg.AllowProduction); err != nil {
		return nil, "", err
	}
	d, err := db.Open(ctx, dsn)
	if err != nil {
		return nil, "", err
	}
	return d, dsn, nil
}
