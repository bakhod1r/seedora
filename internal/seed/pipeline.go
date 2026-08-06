package seed

import (
	"iter"
	"runtime"
	"sync"

	"github.com/bakhod1r/seedora/internal/db"
)

// producer turns batch generation into a db.Source: a stream of rows the driver
// pulls at wire speed while generation runs ahead of it on its own goroutines.
//
// The shape of the problem is that generating a row costs real CPU — locale
// lookups, format-valid identifiers, uniqueness checks — while writing it costs
// a database round trip that one core can saturate. Running them in lockstep
// wastes whichever is faster. Running generation on a pool, ahead of the writer,
// means the database sees a continuous stream and never waits on us.
//
// Determinism survives the parallelism because a batch's contents depend only on
// its index, and finished batches are re-ordered by index before they are
// yielded. Two runs with the same seed produce the same rows in the same order,
// whatever the workers happened to do.
type producer struct {
	batches int
	size    int
	total   int

	// gen produces one batch. It must be safe to call from several goroutines
	// and must depend on nothing but its arguments — that is what makes the
	// output independent of scheduling.
	gen func(index, offset, n int) ([]map[string]any, error)

	// ahead is how many finished or in-flight batches may run in front of the
	// writer. It bounds memory at roughly ahead*size rows.
	ahead int

	mu  sync.Mutex
	err error
}

func newProducer(total, size int, gen func(index, offset, n int) ([]map[string]any, error)) *producer {
	if size <= 0 {
		size = 5000
	}
	return &producer{
		batches: (total + size - 1) / size,
		size:    size,
		total:   total,
		gen:     gen,
		ahead:   workers() * 2,
	}
}

// workers is the generation pool size. Generation is pure CPU with no I/O, so
// one goroutine per core is the right number; more only adds scheduling.
func workers() int {
	n := runtime.GOMAXPROCS(0)
	switch {
	case n < 1:
		return 1
	case n > 16:
		// Past a point the writer is the limit and every extra worker is
		// another batch held in memory for nothing.
		return 16
	}
	return n
}

// Rows implements db.Source.
func (p *producer) Rows() iter.Seq[map[string]any] {
	return func(yield func(map[string]any) bool) {
		if p.batches == 0 {
			return
		}

		type result struct {
			rows []map[string]any
			err  error
		}

		// slots is the reorder buffer. A worker parks its finished batch in the
		// slot for its index; the writer takes slots in index order. Each slot
		// holds one batch, so a worker never waits on another worker.
		slots := make([]chan result, p.batches)
		for i := range slots {
			slots[i] = make(chan result, 1)
		}

		// permits is the backpressure. The dispatcher takes one before handing
		// out a batch and the writer returns one after consuming a batch, so at
		// most `ahead` batches are ever in flight. Without it, a fast generator
		// on a slow database would build the entire table in memory.
		permits := make(chan struct{}, p.ahead)
		for range p.ahead {
			permits <- struct{}{}
		}

		// done stops the dispatcher and the workers when the writer leaves
		// early — a failed COPY, or a consumer that breaks out of the range.
		done := make(chan struct{})
		var closeOnce sync.Once
		stop := func() { closeOnce.Do(func() { close(done) }) }

		next := make(chan int)
		var wg sync.WaitGroup

		for range workers() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range next {
					offset := i * p.size
					n := min(p.size, p.total-offset)
					rows, err := p.gen(i, offset, n)
					select {
					case slots[i] <- result{rows: rows, err: err}:
					case <-done:
						return
					}
				}
			}()
		}

		go func() {
			defer close(next)
			for i := range p.batches {
				select {
				case <-permits:
				case <-done:
					return
				}
				select {
				case next <- i:
				case <-done:
					return
				}
			}
		}()

		// Whatever happens below, unwind the pipeline before returning: signal
		// everyone to stop, then wait for the workers so no goroutine outlives
		// the call. Waiting matters — these goroutines hold batches, and a run
		// that leaked one per table would grow without bound.
		defer func() {
			stop()
			wg.Wait()
		}()

		for i := range p.batches {
			var res result
			select {
			case res = <-slots[i]:
			case <-done:
				return
			}
			if res.err != nil {
				p.setErr(res.err)
				return
			}
			for _, row := range res.rows {
				if !yield(row) {
					return
				}
			}
			// This batch is written; let another start.
			select {
			case permits <- struct{}{}:
			default:
			}
		}
	}
}

// Err implements db.Source.
func (p *producer) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *producer) setErr(err error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.mu.Unlock()
}

// counted wraps a source and reports progress as rows go past.
type counted struct {
	src  db.Source
	each func(n int)
	// every is how many rows between callbacks. Reporting per row would cost
	// more than generating one.
	every int
}

func (c *counted) Rows() iter.Seq[map[string]any] {
	return func(yield func(map[string]any) bool) {
		n, reported := 0, 0
		for row := range c.src.Rows() {
			if !yield(row) {
				return
			}
			n++
			if c.each != nil && c.every > 0 && n%c.every == 0 {
				c.each(n)
				reported = n
			}
		}
		// The final call closes out the table, unless the last periodic one
		// already landed on exactly the last row — reporting the same count
		// twice makes the CLI print its completion line twice.
		if c.each != nil && reported != n {
			c.each(n)
		}
	}
}

func (c *counted) Err() error { return c.src.Err() }
