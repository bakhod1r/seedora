package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/bakhod1r/seedora/internal/seed"
)

// A seeding run is started by one request and watched by another.
//
// The alternative — one long POST that returns when the run finishes — gives the
// page nothing to show for the minute it takes to write a million rows, and dies
// with any proxy that has an idle timeout. Splitting it means the browser can
// draw a progress bar per table, which is the difference between a tool that
// looks stuck and one that looks like it is working.

// runEvent is one message on the stream. The name maps to an SSE event type, so
// the client attaches a handler per kind rather than switching on a field.
type runEvent struct {
	Name string
	Data any
}

// progressData is emitted as rows go past.
type progressData struct {
	Table   string `json:"table"`
	Written int    `json:"written"`
	Total   int    `json:"total"`
	Index   int    `json:"index"`
	Count   int    `json:"count"`
}

// run is a single seeding run and everyone watching it.
type run struct {
	id string

	mu sync.Mutex
	// history is every event so far. A watcher that connects late — or
	// reconnects — is replayed it, so the page never shows a run that appears to
	// start halfway through.
	history []runEvent
	subs    map[chan runEvent]struct{}
	done    bool
}

func newRun() *run {
	return &run{
		id:   fmt.Sprintf("%d", time.Now().UnixNano()),
		subs: map[chan runEvent]struct{}{},
	}
}

// emit records an event and hands it to every watcher.
func (r *run) emit(name string, data any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return
	}
	ev := runEvent{Name: name, Data: data}
	r.history = append(r.history, ev)
	if name == "done" || name == "failed" {
		r.done = true
	}
	for ch := range r.subs {
		// Non-blocking: a watcher that has stopped reading must not stall the
		// run that is writing to the database.
		select {
		case ch <- ev:
		default:
		}
	}
	if r.done {
		for ch := range r.subs {
			close(ch)
		}
		r.subs = map[chan runEvent]struct{}{}
	}
}

// subscribe returns the events so far plus a channel of what follows. A closed
// channel means the run has finished.
func (r *run) subscribe() ([]runEvent, chan runEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()

	history := append([]runEvent(nil), r.history...)
	if r.done {
		ch := make(chan runEvent)
		close(ch)
		return history, ch
	}
	// Buffered deeply enough that a browser stalling for a moment does not lose
	// the progress it was about to draw.
	ch := make(chan runEvent, 256)
	r.subs[ch] = struct{}{}
	return history, ch
}

func (r *run) unsubscribe(ch chan runEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.subs[ch]; ok {
		delete(r.subs, ch)
		close(ch)
	}
}

// handleSeed starts a run and returns its id. It does not wait for the run to
// finish; the page watches /api/seed/events.
func (s *Server) handleSeed(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DryRun   bool   `json:"dry_run"`
		Truncate bool   `json:"truncate"`
		Append   bool   `json:"append"`
		Rows     int    `json:"rows"`
		Seed     uint64 `json:"seed"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	s.mu.Lock()
	if s.driver == nil {
		s.mu.Unlock()
		writeErr(w, http.StatusConflict, errNotConnected)
		return
	}
	if s.run != nil && !s.run.finished() {
		s.mu.Unlock()
		writeErr(w, http.StatusConflict, errRunning)
		return
	}
	active := newRun()
	s.run = active
	s.running = true
	d, sc, p := s.driver, s.schema, s.plan
	opts := seed.Options{
		Seed:     req.Seed,
		Locale:   s.cfg.Locale,
		Rows:     req.Rows,
		Batch:    s.cfg.Batch,
		Truncate: req.Truncate,
		Append:   req.Append,
		DryRun:   req.DryRun,
	}
	s.mu.Unlock()

	// The run outlives the request that started it, so it gets a context of its
	// own — cancelling it with the request would kill the run the moment the
	// browser got its reply.
	ctx := context.WithoutCancel(r.Context())

	go func() {
		opts.Progress = func(pr seed.Progress) {
			active.emit("progress", progressData{
				Table:   pr.Table,
				Written: pr.Written,
				Total:   pr.Total,
				Index:   pr.TableIndex,
				Count:   pr.TableCount,
			})
		}

		res, err := seed.Run(ctx, d, sc, p, opts)

		s.mu.Lock()
		s.running = false
		if err == nil && !req.DryRun {
			// Row counts are now stale, and the number the truncate warning
			// shows has to be the real one.
			if fresh, ierr := d.Introspect(ctx); ierr == nil {
				restoreOrder(s.plan, fresh)
				s.schema = fresh
			}
		}
		if err == nil {
			s.last = res
		}
		s.mu.Unlock()

		if err != nil {
			active.emit("failed", map[string]string{"error": err.Error()})
			return
		}
		active.emit("done", res)
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"run_id": active.id})
}

func (r *run) finished() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done
}

// handleSeedEvents streams the current run as Server-Sent Events.
//
// SSE rather than a WebSocket: the traffic is one-way, it is plain HTTP so it
// works through anything, and the browser reconnects on its own. A WebSocket
// would be a second protocol to carry strictly less.
func (s *Server) handleSeedEvents(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	active := s.run
	s.mu.Unlock()

	if active == nil {
		writeErr(w, http.StatusNotFound, errNoRun)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errNoStreaming)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Proxies that buffer would hold every event until the run ended, which is
	// the one thing this endpoint exists to avoid.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	history, ch := active.subscribe()
	defer active.unsubscribe(ch)

	send := func(ev runEvent) bool {
		b, err := json.Marshal(ev.Data)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	for _, ev := range history {
		if !send(ev) {
			return
		}
	}

	// A heartbeat keeps an idle connection from being closed by anything in the
	// middle during the gap between two slow tables.
	beat := time.NewTicker(15 * time.Second)
	defer beat.Stop()

	for {
		select {
		case ev, open := <-ch:
			if !open {
				return
			}
			if !send(ev) {
				return
			}
		case <-beat.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
