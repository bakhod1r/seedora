// Package seed executes a plan against a database.
//
// The run is one transaction from the first row to the last, so a failure
// anywhere leaves the database exactly as it was. Within it, tables are written
// parents first, and a child's foreign keys are drawn from the parent keys this
// run actually wrote — not from a guess at what the parent ids will be.
//
// Generation runs on a worker pool ahead of the writer, and each table is a
// single bulk write, so the database's only job is to take rows off the wire.
package seed

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/bakhod1r/synth"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/model"
	"github.com/bakhod1r/seedora/internal/plan"
)

// Options tune one run.
type Options struct {
	// Seed fixes the RNG. Zero means "pick one from the clock", and the value
	// actually used is reported in the Result, so a run can be reproduced after
	// the fact.
	Seed uint64
	// Locale overrides the plan's locale.
	Locale string
	// Rows overrides the row count for every table.
	Rows int
	// Batch is how many rows one worker generates at a time. It is a unit of
	// work, not a unit of writing: the table is still one bulk statement.
	Batch int
	// Truncate empties every target table first, whatever the plan says.
	Truncate bool
	// Append adds rows to tables that already hold some, instead of assuming
	// the table is empty. No table is emptied — not even one the plan asks to
	// truncate — and every unique column is read back first so the rows this
	// run generates are unique against the ones already there.
	Append bool
	// AppendUniqueCap is how many existing values --append will hold in memory
	// per unique text column. Zero takes the default. It is a memory limit, so
	// raising it is the caller's call to make on a machine with the headroom.
	AppendUniqueCap int
	// TxPerTable commits after each table instead of wrapping the whole run in
	// one transaction.
	//
	// The single transaction is the right default: a failure anywhere leaves
	// the database as it was. It stops being right at scale — a hundred million
	// rows in one transaction grows the WAL without bound, holds off autovacuum
	// for the duration, and turns a failure in the last table into a rollback
	// of everything that came before it. This trades that atomicity for a run
	// that can be resumed with --append.
	TxPerTable bool
	// DryRun generates and validates everything but writes nothing.
	DryRun bool
	// Progress is called as rows go past. It may be nil, and it is called from
	// the writing goroutine, so it must not block.
	Progress func(Progress)
}

// Progress is one update during a run.
type Progress struct {
	Table      string
	Written    int
	Total      int
	TableIndex int
	TableCount int
}

// Result is what a finished run produced.
type Result struct {
	Seed     uint64        `json:"seed"`
	Tables   []TableResult `json:"tables"`
	Rows     int64         `json:"rows"`
	Duration time.Duration `json:"duration"`
	DryRun   bool          `json:"dry_run"`
	// Verified is true when a dry run reached the database: the rows were
	// written inside the transaction and the transaction was rolled back, so
	// every constraint the server enforces was exercised. False on an engine
	// whose transaction cannot be undone, where a dry run stops at generation
	// and proves nothing about what the database would have said.
	Verified bool `json:"verified,omitempty"`
}

// RowsPerSecond is the throughput the run achieved.
func (r *Result) RowsPerSecond() float64 {
	if r.Duration <= 0 {
		return 0
	}
	return float64(r.Rows) / r.Duration.Seconds()
}

// TableResult is one table's outcome.
type TableResult struct {
	Table string `json:"table"`
	Rows  int64  `json:"rows"`
}

// Run executes the plan. The caller owns the driver; Run owns the transaction.
func Run(ctx context.Context, d db.Driver, s *model.Schema, p *plan.Plan, opts Options) (_ *Result, err error) {
	if errs := p.Validate(s); len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	// The two flags mean opposite things about the rows already in the table,
	// and there is no reading of "empty it and then add to what is in it" that
	// is worth guessing at.
	if opts.Append && opts.Truncate {
		return nil, errors.New("--append and --truncate contradict each other: " +
			"one keeps the rows already in the table, the other deletes them")
	}
	order, err := Order(s, p)
	if err != nil {
		return nil, err
	}
	if opts.Batch <= 0 {
		opts.Batch = 5000
	}
	seedVal := firstNonZero(opts.Seed, p.Seed, uint64(time.Now().UnixNano()))
	locale := opts.Locale
	if locale == "" {
		locale = p.Locale
	}

	start := time.Now()
	tx, err := d.Begin(ctx)
	if err != nil {
		return nil, err
	}
	// Unconditional: Commit marks the transaction done, so this is a no-op on
	// the success path and the only thing that runs on every failure path.
	//
	// Its error is joined onto the failure rather than discarded. On an engine
	// that commits as it writes — ClickHouse, Snowflake, BigQuery and the rest —
	// Rollback undoes nothing and says so, naming what is already permanent.
	// That sentence is the whole point of not faking a transaction, and it is
	// worth nothing if the only caller drops it. Only a failing run is
	// augmented: after a successful Commit the transaction is done and this
	// returns nil.
	defer func() {
		if rerr := tx.Rollback(ctx); rerr != nil && err != nil {
			err = fmt.Errorf("%w\n%w", err, rerr)
		}
	}()

	r := &Result{Seed: seedVal, DryRun: opts.DryRun}

	// A dry run that can be rolled back writes like any other run: the rows go
	// to the database and the transaction is undone at the end. Everything below
	// keys off this rather than off DryRun, because a dry run that writes has to
	// truncate, resolve real parent keys, and be unique against what is there.
	writing := !opts.DryRun || db.CanRollBack(tx)
	r.Verified = opts.DryRun && writing

	// Truncation runs for every table before any table is written, in reverse
	// dependency order. Doing it as we go would delete a parent's rows after
	// its children already pointed at them.
	// --append overrides the plan as well as the flag. A per-table Truncate in
	// seedora.yaml is a statement about a run that starts from empty, and this
	// one does not.
	if writing && !opts.Append {
		for i := len(order) - 1; i >= 0; i-- {
			t := order[i]
			if !opts.Truncate && !p.Tables[t.Name].Truncate {
				continue
			}
			if err := tx.Truncate(ctx, t); err != nil {
				return nil, err
			}
		}
	}

	// keys caches parent key columns read back within this transaction, so a
	// parent referenced by five children costs one query, not five.
	keys := map[string][]any{}

	if opts.TxPerTable && !opts.DryRun {
		// The truncations were done in the transaction opened above and have to
		// be permanent before the first table is written into its own.
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit: %w", err)
		}
	}

	for i, t := range order {
		tp := p.Tables[t.Name]
		rows := tp.Rows
		if opts.Rows > 0 {
			rows = opts.Rows
		}
		if rows <= 0 {
			continue
		}

		tableTx := tx
		if opts.TxPerTable && !opts.DryRun {
			// A parent committed by the previous iteration is what this one
			// reads its foreign keys from, and the cache built inside the old
			// transaction is still valid: the keys were committed, not undone.
			tableTx, err = d.Begin(ctx)
			if err != nil {
				return nil, err
			}
		}

		n, err := seedTable(ctx, tableTx, s, t, tp, locale, seedVal, rows, i, len(order), keys, opts)
		if err != nil {
			if tableTx != tx {
				_ = tableTx.Rollback(ctx)
			}
			return nil, fmt.Errorf("%s: %w", t.Name, err)
		}
		if tableTx != tx {
			if err := tableTx.Commit(ctx); err != nil {
				return nil, fmt.Errorf("%s: commit: %w", t.Name, err)
			}
		}
		r.Tables = append(r.Tables, TableResult{Table: t.Name, Rows: n})
		r.Rows += n
	}

	if opts.DryRun {
		r.Duration = time.Since(start)
		return r, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	r.Duration = time.Since(start)
	return r, nil
}

// seedTable generates and writes one table as a single bulk write.
func seedTable(
	ctx context.Context,
	tx db.Tx,
	s *model.Schema,
	t *model.Table,
	tp *plan.TablePlan,
	locale string,
	baseSeed uint64,
	rows, index, total int,
	keys map[string][]any,
	opts Options,
) (int64, error) {
	cols, err := writableColumns(t, tp)
	if err != nil || len(cols) == 0 {
		return 0, err
	}

	// A dry run only stops at generation where the transaction cannot be undone.
	// Everywhere else it writes and is rolled back, which is the only way to
	// learn what the database thinks of the rows.
	generateOnly := opts.DryRun && !db.CanRollBack(tx)

	gen, err := newGenerator(t, tp, locale, baseSeed, opts.Batch)
	if err != nil {
		return 0, err
	}

	// Foreign keys resolve once per table: every batch draws from the same
	// parent pool, which keeps children spread evenly over parents rather than
	// clustered by batch, and costs one query per referenced column.
	// A dry run that writes has real parent rows to point at, in its own
	// transaction, so it resolves foreign keys exactly as a real run does. Only
	// the dry run that never wrote needs a stand-in pool.
	fks, err := resolveFKs(ctx, tx, s, t, tp, keys, rows, generateOnly)
	if err != nil {
		return 0, err
	}
	// A one-to-one child cannot have more rows than there are parents to point
	// at. Caught here rather than at the write, where it arrives as a duplicate
	// key hundreds of thousands of rows in.
	for col, cp := range tp.Columns {
		if cp.Skip || cp.Generator != plan.GenForeignKey || !cp.Unique {
			continue
		}
		if n := len(fks[col]); n > 0 && n < rows {
			return 0, fmt.Errorf(
				"column %s is a unique foreign key, so %d rows need %d distinct parents but only %d exist",
				col, rows, rows, n)
		}
	}
	gen.fks = fks
	gen.cols = cols

	var src db.Source = newProducer(rows, opts.Batch, gen.batch)

	// A composite primary key made of foreign keys — a join table — constrains
	// the combination rather than either column, which is the one thing the
	// per-column uniqueness pass cannot repair. Its pairs are assigned from the
	// parent pools instead of drawn.
	comp, err := newComposite(t, tp, fks, rows, derive(baseSeed, t.Name))
	if err != nil {
		return 0, err
	}
	if comp != nil {
		// A join table's uniqueness is over the pair, and the pairs already in
		// the table cannot be read back one column at a time — which is the
		// only shape of read the driver interface has. Appending would generate
		// pairs from the same parent pools as the previous run and collide with
		// it, so this is refused where it can be explained rather than left to
		// surface as a constraint violation with a run half written.
		if opts.Append && !generateOnly {
			return 0, errors.New("--append is not supported on a join table: its " +
				"uniqueness is over the pair of foreign keys, and the pairs already " +
				"present cannot be read back to generate around them. Seed it in one " +
				"run, or exclude it with rows: 0")
		}
		src = &compositeSource{src: src, set: comp}
	}

	// Uniqueness is enforced here rather than inside a worker: it is the one
	// piece of per-table state that every row touches, and a lock around it on
	// the generating side would serialise the pool it exists to parallelise.
	// On this side it is already sequential and costs a map lookup per value.
	if u := newUniqueSet(t, tp, rows); u.active() {
		// Appending has to be unique against the table, not only against the
		// run, so the column is read back before the first row is generated.
		// A dry run that never writes has nothing to be unique against; one that
		// does is unique against the same rows a real run would face.
		if opts.Append && !generateOnly {
			if err := u.preload(ctx, tx, t, opts.AppendUniqueCap); err != nil {
				return 0, err
			}
		}
		src = &uniqueSource{src: src, set: u}
	}

	if opts.Progress != nil {
		src = &counted{
			src:   src,
			every: opts.Batch,
			each: func(n int) {
				opts.Progress(Progress{
					Table: t.Name, Written: n, Total: rows,
					TableIndex: index, TableCount: total,
				})
			},
		}
	}

	// A dry run on an engine that can undo the write does write: the rows go
	// down the wire inside the transaction and the transaction is rolled back.
	// Everything the database enforces — CHECK constraints, partial and
	// expression unique indexes, foreign keys, the encoding of every value into
	// the column's type — is enforced on the server and nowhere else, so a run
	// that stops short of the wire has checked none of it and still reports
	// success. That is the failure this flag exists to prevent.
	if generateOnly {
		return drain(src)
	}
	return tx.Insert(ctx, t, cols, src)
}

// drain consumes a source without writing. It is what --dry-run falls back to
// on an engine whose transaction cannot be undone, where writing the rows would
// seed the database rather than pretend to. Generation, foreign-key resolution
// and uniqueness still run; nothing the server enforces does.
func drain(src db.Source) (int64, error) {
	var n int64
	for range src.Rows() {
		n++
	}
	return n, src.Err()
}

// generator produces one batch of rows for a table. It holds no mutable state
// that batches share, so several goroutines can call batch concurrently and the
// result depends only on the batch index.
type generator struct {
	table  string
	tp     *plan.TablePlan
	cols   []string
	locale string
	seed   uint64
	fks    map[string][]any

	// specs hands each goroutine its own compiled spec. Synth's spec carries a
	// row count that generation mutates, so one shared instance would race.
	// Compiling is cheap next to generating a batch, and pooling means it
	// happens once per worker rather than once per batch.
	specs *sync.Pool
	// hasSynth is false when every column is filled by Seedora itself, in which
	// case Synth is never called at all.
	hasSynth bool

	// seqText marks the sequence columns whose type is text, so the counter is
	// written as a string rather than as an integer the column cannot hold.
	seqText map[string]bool

	// patterns holds the compiled regex generator for every pattern column, so
	// the expression is parsed once per table rather than once per row.
	patterns map[string]*patternGen

	// clamp is the declared character limit per text column. A generator knows
	// what an email looks like but not that this particular column holds 30
	// characters, and a value one character over is rejected by the database
	// after the run is already half done.
	clamp map[string]int
}

func newGenerator(t *model.Table, tp *plan.TablePlan, locale string, baseSeed uint64, batch int) (*generator, error) {
	// Each table gets its own seed derived from the run's, so adding a table to
	// a plan does not shift the data every other table generates.
	tableSeed := derive(baseSeed, t.Name)

	specYAML, _, err := renderSpec(t.Name, tp, locale, tableSeed)
	if err != nil {
		return nil, err
	}

	g := &generator{
		table:    t.Name,
		tp:       tp,
		locale:   localeOr(locale),
		seed:     tableSeed,
		clamp:    textLimits(t, tp),
		patterns: map[string]*patternGen{},
		seqText:  map[string]bool{},
	}
	for _, c := range t.Columns {
		cp := tp.Get(c.Name)
		if cp == nil || cp.Skip || cp.Generator != plan.GenSequence {
			continue
		}
		switch plan.Classify(c.Type) {
		case plan.ClassString, plan.ClassUUID:
			g.seqText[c.Name] = true
		}
	}
	for col, cp := range tp.Columns {
		if cp.Skip || cp.Generator != plan.GenPattern {
			continue
		}
		// Compiled here for the same reason the Synth spec is: a bad expression
		// must fail before the first row, not on the first batch.
		pg, err := newPatternGen(cp.Pattern)
		if err != nil {
			return nil, fmt.Errorf("column %s: %w", col, err)
		}
		g.patterns[col] = pg
	}
	if specYAML == nil {
		return g, nil
	}

	// Compile once here so a broken spec fails before any row is written,
	// rather than inside a worker on the first batch.
	if _, err := synth.YAMLBytes(specYAML); err != nil {
		return nil, fmt.Errorf("compile generators: %w", err)
	}
	g.hasSynth = true
	g.specs = &sync.Pool{New: func() any {
		spec, err := synth.YAMLBytes(specYAML)
		if err != nil {
			// Unreachable: the same bytes compiled a moment ago.
			return err
		}
		return spec
	}}
	return g, nil
}

// batch generates rows [offset, offset+n) of the table.
func (g *generator) batch(index, offset, n int) ([]map[string]any, error) {
	var rows []map[string]any

	if g.hasSynth {
		v := g.specs.Get()
		if err, bad := v.(error); bad {
			return nil, err
		}
		spec := v.(*synth.YAMLSpec)
		defer g.specs.Put(spec)

		spec.SetCount(n)
		// Offset is what makes batch two continue where batch one stopped
		// instead of repeating it: Synth seeds each row from its index, so the
		// batch's contents follow from its offset and nothing else.
		var err error
		rows, err = spec.Generate(
			synth.WithSeed(g.seed),
			synth.WithLocale(g.locale),
			synth.Offset(offset),
		)
		if err != nil {
			return nil, fmt.Errorf("generate: %w", err)
		}
		rows = resize(rows, n)
	} else {
		rows = make([]map[string]any, n)
		for i := range rows {
			rows[i] = make(map[string]any, len(g.cols))
		}
	}

	// The batch's own RNG is seeded from its index, so foreign-key draws and
	// null placement are reproducible no matter which worker ran it or when.
	r := rand.New(rand.NewPCG(g.seed, uint64(index)+0x9e3779b97f4a7c15))
	g.fill(rows, offset, r)
	return rows, nil
}

// fill applies everything Synth does not: foreign keys, sequences, constants,
// nulls, and weighted booleans.
func (g *generator) fill(rows []map[string]any, offset int, r *rand.Rand) {
	for i, row := range rows {
		for _, col := range g.cols {
			cp := g.tp.Columns[col]
			if cp == nil {
				continue
			}
			switch cp.Generator {
			case plan.GenForeignKey:
				pool := g.fks[col]
				switch {
				case len(pool) == 0:
					row[col] = nil
				case cp.Unique:
					// One-to-one: parents are handed out in order, so every
					// child gets a different one and every parent up to the
					// child count gets exactly one child. A random draw here
					// would collide long before the pool ran out — the
					// birthday problem, and the column is unique.
					row[col] = pool[(offset+i)%len(pool)]
				default:
					row[col] = pool[r.IntN(len(pool))]
				}
			case plan.GenSequence:
				start := int64(1)
				if cp.Start != nil {
					start = *cp.Start
				}
				n := start + int64(offset+i)
				// A counter into a text column is written as text. The column
				// decides, not the generator: `provider_uid VARCHAR` holds
				// "1000", and sending the integer is a wire-level encoding
				// error that stops the load rather than a value the column
				// refuses.
				if g.seqText[col] {
					row[col] = strconv.FormatInt(n, 10)
				} else {
					row[col] = n
				}
			case plan.GenPattern:
				if p := g.patterns[col]; p != nil {
					row[col] = p.generate(r)
				}
			case plan.GenNull:
				row[col] = nil
			case plan.GenConst:
				row[col] = cp.Const
			}

			// A weighted boolean is Seedora's own because Synth's bool kind is
			// a fair coin, and the useful case — 85% active users — is not.
			if cp.TrueWeight != nil {
				row[col] = r.Float64() < *cp.TrueWeight
			}

			// Nulls last, so they override whatever was generated.
			if cp.NullRate > 0 && r.Float64() < cp.NullRate {
				row[col] = nil
				continue
			}

			if limit, bounded := g.clamp[col]; bounded {
				row[col] = clampText(row[col], limit)
			}
		}
	}
}

// textLimits collects the character limit of every text column the plan fills.
func textLimits(t *model.Table, tp *plan.TablePlan) map[string]int {
	out := map[string]int{}
	for _, c := range t.Columns {
		cp := tp.Get(c.Name)
		if cp == nil || cp.Skip || c.MaxLen <= 0 {
			continue
		}
		if cp.Generator == plan.GenPattern {
			// A pattern's whole job is to match, and a truncated match does
			// not. The regex carries its own length bounds; the column's are
			// checked against them at planning time instead.
			continue
		}
		if plan.Classify(c.Type) == model.ClassString {
			out[c.Name] = c.MaxLen
		}
	}
	return out
}

// clampText truncates a value to the column's declared width, counting runes so
// a multi-byte character is never cut in half — half a rune is not text, and
// some engines reject it outright.
func clampText(v any, limit int) any {
	s, ok := v.(string)
	if !ok || limit <= 0 {
		return v
	}
	if len(s) <= limit {
		return s
	}
	runes := []rune(s)
	for len(runes) > 0 && len(string(runes)) > limit {
		runes = runes[:len(runes)-1]
	}
	return string(runes)
}

// writableColumns is the column list a bulk write uses: catalog order, minus
// everything the plan skips and everything the database computes.
func writableColumns(t *model.Table, tp *plan.TablePlan) ([]string, error) {
	var out []string
	for _, c := range t.Columns {
		cp := tp.Get(c.Name)
		if cp == nil {
			// No plan entry. A column the database fills itself is fine to
			// omit; one it cannot was already rejected by Validate.
			continue
		}
		if cp.Skip || cp.Generator == plan.GenDefault || c.Generated {
			continue
		}
		out = append(out, c.Name)
	}
	return out, nil
}

// resolveFKs builds, per foreign-key column, the pool of parent keys to draw
// from.
func resolveFKs(
	ctx context.Context,
	tx db.Tx,
	s *model.Schema,
	t *model.Table,
	tp *plan.TablePlan,
	cache map[string][]any,
	rows int,
	generateOnly bool,
) (map[string][]any, error) {
	out := map[string][]any{}
	// A cached pool was read for an earlier column and may be smaller than this
	// one needs, so a one-to-one column re-reads rather than shares.
	exact := map[string]bool{}
	for col, cp := range tp.Columns {
		if cp.Skip || cp.Generator != plan.GenForeignKey {
			continue
		}
		ref := cp.References
		if ref == "" {
			c := t.Column(col)
			if c == nil || c.FK == nil {
				continue
			}
			ref = c.FK.Table + "." + c.FK.Column
		}
		parentName, parentCol, ok := plan.SplitRef(ref)
		if !ok {
			return nil, fmt.Errorf("column %s: bad reference %q", col, ref)
		}
		// The pool is capped to bound memory on a parent with millions of rows.
		// A one-to-one child is the exception: it needs one parent per row, so
		// the cap has to clear its row count or the run cannot succeed.
		limit := fkPoolCap
		if cp.Unique && rows > limit {
			limit = rows
		}
		if k, hit := cache[ref]; hit && (len(k) >= limit || !cp.Unique || exact[ref]) {
			out[col] = k
			continue
		}
		parent := s.Table(parentName)
		if parent == nil {
			return nil, fmt.Errorf("column %s references unknown table %s", col, parentName)
		}
		// Drawing from a large sample rather than the whole set changes nothing
		// about referential integrity and keeps the pool in cache.
		k, err := tx.ReadKeys(ctx, parent, parentCol, limit)
		if err != nil {
			return nil, err
		}
		if cp.Unique {
			exact[ref] = true
		}
		if len(k) == 0 && !nullable(t, col) {
			// A dry run that could not write never wrote the parent, so its key
			// column is empty by construction. Standing in a placeholder pool
			// lets the rest of the plan be exercised, which is the point of the
			// flag; every other run fails here, because there it means a
			// genuinely empty parent — and a dry run that does write has to
			// fail, since the values it invented would be rejected anyway.
			if !generateOnly {
				return nil, fmt.Errorf("column %s references %s, which has no rows",
					col, parentName)
			}
			k = placeholderKeys()
		}
		// An empty pool is not cached. Everything else about a parent is
		// settled by the time it is read, but emptiness is not: a table that
		// references itself resolves its own key column before a single row of
		// it has been written, and that read is legitimately empty. Caching it
		// would hand the same empty pool to every later child of that table,
		// which is a NULL in a column the schema says is NOT NULL — and the run
		// then fails two tables further on, naming a column whose plan is
		// perfectly correct.
		if len(k) > 0 {
			cache[ref] = k
		}
		out[col] = k
	}
	return out, nil
}

func nullable(t *model.Table, col string) bool {
	c := t.Column(col)
	return c != nil && c.Nullable
}

// fkPoolCap is how many parent keys are held per reference.
const fkPoolCap = 200_000

// placeholderKeys stands in for a parent a dry run has not written. The values
// are never sent anywhere, so any non-empty pool does.
func placeholderKeys() []any {
	out := make([]any, 100)
	for i := range out {
		out[i] = int64(i + 1)
	}
	return out
}

// resize trims or pads a generated batch to exactly n rows. Synth generates the
// count the spec asks for, so this only ever trims — the pad path exists so a
// future change to that cannot silently write short.
func resize(batch []map[string]any, n int) []map[string]any {
	if len(batch) > n {
		return batch[:n]
	}
	for len(batch) < n {
		batch = append(batch, map[string]any{})
	}
	return batch
}

// compositeSource assigns each row its combination of parent keys, in order.
type compositeSource struct {
	src db.Source
	set *composite
}

func (c *compositeSource) Rows() iter.Seq[map[string]any] {
	return func(yield func(map[string]any) bool) {
		i := 0
		for row := range c.src.Rows() {
			c.set.assign(row, i)
			if !yield(row) {
				return
			}
			i++
		}
	}
}

func (c *compositeSource) Err() error { return c.src.Err() }

// uniqueSource enforces uniqueness as rows pass through, in order.
type uniqueSource struct {
	src db.Source
	set *uniqueSet
	err error
}

func (u *uniqueSource) Rows() iter.Seq[map[string]any] {
	return func(yield func(map[string]any) bool) {
		i := 0
		for row := range u.src.Rows() {
			if err := u.set.enforce(row, i); err != nil {
				u.err = err
				return
			}
			if !yield(row) {
				return
			}
			i++
		}
	}
}

func (u *uniqueSource) Err() error {
	if u.err != nil {
		return u.err
	}
	return u.src.Err()
}

// derive mixes a table name into the run seed with FNV-1a, so every table has a
// distinct, stable stream.
func derive(seed uint64, name string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64) ^ seed
	for i := 0; i < len(name); i++ {
		h ^= uint64(name[i])
		h *= prime64
	}
	if h == 0 {
		return 1
	}
	return h
}

func firstNonZero(vals ...uint64) uint64 {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 1
}
