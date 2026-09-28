package ui_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bakhod1r/seedora/internal/config"
	"github.com/bakhod1r/seedora/internal/ui"
)

const tok = "k3Qx9vT2mB7wL0pR8sYcAbCdEfGhIjKl"

func tokenServer(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SEEDORA_CONFIG_DIR", filepath.Join(dir, "config"))
	cfg := &config.Config{ConfigPath: filepath.Join(dir, "seedora.yaml"), Locale: "en_US", Host: "127.0.0.1", Port: 7777, Token: tok}
	return ui.New(cfg, nil, "", nil, nil, false).Handler()
}

func send(h http.Handler, method, target string, mod func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.Host = "127.0.0.1:7777"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if mod != nil {
		mod(req)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// The API holds DSNs and can truncate tables. Without the launch token nobody
// reaches it — not a curl from the network, not another local user.
func TestAPIRequiresToken(t *testing.T) {
	h := tokenServer(t)
	if w := send(h, http.MethodGet, "/api/state", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/state without a token: %d, want 401", w.Code)
	}
	if w := send(h, http.MethodGet, "/api/state", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "seedora_token", Value: "wrong"})
	}); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong cookie: %d, want 401", w.Code)
	}
	if w := send(h, http.MethodGet, "/api/state", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+tok)
	}); w.Code != http.StatusOK {
		t.Fatalf("bearer token: %d, want 200", w.Code)
	}
}

// Opening the printed link signs the tab in with an HttpOnly cookie and takes
// the token out of the address bar.
func TestLinkTokenSetsCookieAndRedirects(t *testing.T) {
	h := tokenServer(t)
	w := send(h, http.MethodGet, "/?token="+tok, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("status %d Location %q, want 303 to /", w.Code, w.Header().Get("Location"))
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != tok || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie = %+v, want HttpOnly SameSite=Strict token cookie", cookies)
	}
	c := cookies[0]
	if w := send(h, http.MethodGet, "/api/state", func(r *http.Request) { r.AddCookie(c) }); w.Code != http.StatusOK {
		t.Fatalf("API with the cookie: %d, want 200", w.Code)
	}
	if w := send(h, http.MethodGet, "/", func(r *http.Request) { r.AddCookie(c) }); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<") {
		t.Fatalf("page with the cookie: %d", w.Code)
	}
}

func TestPageWithoutTokenIsLocked(t *testing.T) {
	h := tokenServer(t)
	w := send(h, http.MethodGet, "/", nil)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "Open Seedora from its link") {
		t.Fatalf("bare page: %d %q", w.Code, w.Body.String())
	}
	w = send(h, http.MethodGet, "/?token=stale", nil)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "That link has expired") {
		t.Fatalf("stale link: %d", w.Code)
	}
	if w := send(h, http.MethodGet, "/app.js", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("asset without a token: %d, want 401", w.Code)
	}
}
