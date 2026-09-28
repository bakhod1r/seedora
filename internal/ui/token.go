package ui

import (
	"crypto/subtle"
	"html/template"
	"net/http"
	"strings"
)

// tokenCookie carries the launch token after the printed link has been opened.
const tokenCookie = "seedora_token"

// requireToken lets a request through only with the per-launch token: from
// the HttpOnly cookie the printed link sets, or an Authorization: Bearer
// header for scripts. The origin guard stops other websites; this stops
// everyone else who can reach the port — a machine on the network when bound
// to 0.0.0.0, or another user on this one.
//
// Opening /?token=… sets the cookie and redirects to the same path without
// the token, so it does not stay in the address bar or the history.
func requireToken(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte(token)
	match := func(got string) bool {
		return got != "" && subtle.ConstantTimeCompare([]byte(got), want) == 1
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query().Get("token"); q != "" {
			if !match(q) {
				locked(w, true)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     tokenCookie,
				Value:    token,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
			u := *r.URL
			q := u.Query()
			q.Del("token")
			u.RawQuery = q.Encode()
			http.Redirect(w, r, u.RequestURI(), http.StatusSeeOther)
			return
		}
		if c, err := r.Cookie(tokenCookie); err == nil && match(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && match(bearer) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeErr(w, http.StatusUnauthorized, errUnauthorized)
			return
		}
		locked(w, false)
	})
}

var lockedPage = template.Must(template.New("locked").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Seedora</title>
<style>
:root{color-scheme:light dark;--bg:#0d0f12;--surface:#15181d;--border:#262b33;--fg:#e8eaee;--muted:#9aa3b0;--warn:#d1a038;--danger:#d2564c}
@media (prefers-color-scheme: light){:root{--bg:#f6f7f9;--surface:#fff;--border:#dfe3e9;--fg:#16191e;--muted:#5b6472;--warn:#a87a14;--danger:#b8433a}}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;background:var(--bg);color:var(--fg);
font:.8125rem/1.5 ui-sans-serif,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;padding:16px}
.card{max-width:360px;background:var(--surface);border:1px solid var(--border);border-radius:12px;padding:24px;text-align:center}
.g{display:inline-flex;width:40px;height:40px;border-radius:999px;align-items:center;justify-content:center;border:1px solid;font-weight:700}
h1{font-size:1.0625rem;margin:10px 0 4px}p{color:var(--muted);margin:0 0 8px}code{font-family:ui-monospace,Menlo,monospace}
</style></head><body><main class="card">
{{if .Stale}}<span class="g" style="color:var(--danger)">!</span><h1>That link has expired</h1>
<p>The token did not match. Seedora makes a new one every launch — use the link from the terminal you started it in.</p>
{{else}}<span class="g" style="color:var(--warn)">🔒</span><h1>Open Seedora from its link</h1>
<p>This server only answers the tab opened from the link it printed when it started:</p>
<p><code>Open http://…/?token=…</code></p>{{end}}
</main></body></html>`))

func locked(w http.ResponseWriter, stale bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	_ = lockedPage.Execute(w, struct{ Stale bool }{stale})
}
