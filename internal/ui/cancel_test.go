package ui_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bakhod1r/seedora/internal/config"
	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/spec"
	"github.com/bakhod1r/seedora/internal/ui"
)

// Stop ends a run in flight: it reports "cancelled" on the stream, and the
// transaction it rolled back leaves no rows behind.
func TestCancelStopsARunAndWritesNothing(t *testing.T) {
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

	if code, _ := do(t, h, http.MethodPost, "/api/seed/cancel", nil); code != http.StatusNotFound {
		t.Errorf("cancel with no run: status %d, want 404", code)
	}
	if code, body := do(t, h, http.MethodPost, "/api/seed", map[string]any{"rows": 50}); code != http.StatusAccepted {
		t.Fatalf("seed: %d %v", code, body)
	}
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("run never began")
	}
	if code, body := do(t, h, http.MethodPost, "/api/seed/cancel", nil); code != http.StatusAccepted {
		t.Fatalf("cancel: %d %v", code, body)
	}
	close(g.release)

	req := httptest.NewRequest(http.MethodGet, "/api/seed/events", nil)
	req.Host = "127.0.0.1:7777"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "event: cancelled") {
		t.Fatalf("stream did not report the cancel:\n%s", w.Body.String())
	}

	after, err := base.Introspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := after.Table("users").ExistingRows; n != 0 {
		t.Fatalf("a cancelled run left %d rows", n)
	}
}
