package ui_test

import (
	"context"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bakhod1r/seedora/internal/config"
	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/spec"
	"github.com/bakhod1r/seedora/internal/ui"
)

// gateDriver holds the run's first Begin until release is closed, so a test
// can act while a run is in flight. It also counts Begins, which is how a
// Preview touching the shared connection shows up.
type gateDriver struct {
	db.Driver
	release chan struct{}
	begins  atomic.Int32
	entered chan struct{}
}

func (g *gateDriver) Begin(ctx context.Context) (db.Tx, error) {
	if g.begins.Add(1) == 1 {
		close(g.entered)
		<-g.release
	}
	return g.Driver.Begin(ctx)
}

// While a run holds the connection, nothing else may use it: on Postgres it is
// one *pgx.Conn, and a Preview's ROLLBACK on it rolls back the run.
func TestRequestsDoNotShareTheConnectionWithARun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEEDORA_CONFIG_DIR", filepath.Join(dir, "config"))
	dsn := filepath.Join(dir, "test.db")
	ctx := t.Context()
	base, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close(context.Background()) })
	tx, _ := base.Begin(ctx)
	_ = tx.Exec(ctx, "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)")
	_ = tx.Commit(ctx)

	g := &gateDriver{Driver: base, release: make(chan struct{}), entered: make(chan struct{})}
	sc, _ := g.Introspect(ctx)
	cfg := &config.Config{ConfigPath: filepath.Join(dir, "seedora.yaml"), Locale: "en_US", Host: "127.0.0.1", Port: 7777}
	p, loaded, _ := spec.LoadOrInfer(cfg.ConfigPath, sc)
	h := ui.New(cfg, g, dsn, sc, p, loaded).Handler()

	if code, body := do(t, h, http.MethodPost, "/api/seed", map[string]any{"rows": 3}); code != http.StatusAccepted {
		t.Fatalf("seed: %d %v", code, body)
	}
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("run never began")
	}

	if code, _ := do(t, h, http.MethodPost, "/api/preview", map[string]any{"table": "users", "rows": 2}); code != http.StatusConflict {
		t.Errorf("preview during a run: status %d, want 409", code)
	}
	if code, _ := do(t, h, http.MethodGet, "/api/history", nil); code != http.StatusConflict {
		t.Errorf("history during a run: status %d, want 409", code)
	}
	if code, _ := do(t, h, http.MethodPost, "/api/connect", map[string]any{"dsn": filepath.Join(dir, "other.db")}); code != http.StatusConflict {
		t.Errorf("connect during a run: status %d, want 409", code)
	}
	if n := g.begins.Load(); n != 1 {
		t.Errorf("%d Begins while the run held the connection, want only the run's", n)
	}
	close(g.release)
}
