package ui_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// request is the raw form of do(), without the headers a browser on the
// Seedora page would send, so a test can supply its own and see what the guard
// makes of them.
func request(t *testing.T, h http.Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Host = "127.0.0.1:7777"
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// A page on another site can send a request to a loopback port; the only thing
// that stops it acting on a live database is this check.
func TestGuardRefusesCrossSiteRequests(t *testing.T) {
	h := newServer(t, usersDDL)

	// Every route, because a read leaks the schema and the remembered DSNs
	// just as surely as a write changes the database.
	routes := []struct {
		method, path string
	}{
		{"GET", "/api/state"},
		{"GET", "/api/connections"},
		{"GET", "/api/history"},
		{"GET", "/api/export?format=yaml"},
		{"POST", "/api/connect"},
		{"POST", "/api/seed"},
		{"POST", "/api/schema/apply"},
		{"POST", "/api/import"},
		{"POST", "/api/connections/forget"},
	}

	for _, site := range []string{"cross-site", "same-site"} {
		for _, r := range routes {
			w := request(t, h, r.method, r.path, map[string]string{
				"Sec-Fetch-Site": site,
				"Origin":         "http://evil.example",
			})
			if w.Code != http.StatusForbidden {
				t.Errorf("%s %s from a %s page: got %d, want 403",
					r.method, r.path, site, w.Code)
			}
		}
	}
}

// A browser that predates Sec-Fetch-Site still sends Origin on a cross-origin
// request, and that is enough to refuse it.
func TestGuardRefusesForeignOriginWithoutSecFetch(t *testing.T) {
	h := newServer(t, usersDDL)

	w := request(t, h, "POST", "/api/seed", map[string]string{
		"Origin": "http://evil.example",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "cross-origin") {
		t.Errorf("error does not say why: %s", w.Body.String())
	}
}

// The rebinding case: a name the attacker controls, resolved to 127.0.0.1. The
// browser calls it same-origin because it is — of that name — so Sec-Fetch-Site
// and Origin both look clean and only the Host header gives it away.
func TestGuardRefusesRebindingHost(t *testing.T) {
	h := newServer(t, usersDDL)

	w := request(t, h, "POST", "/api/seed", map[string]string{
		"Host":           "rebind.evil.example:7777",
		"Sec-Fetch-Site": "same-origin",
		"Origin":         "http://rebind.evil.example:7777",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "loopback") {
		t.Errorf("error does not say why: %s", w.Body.String())
	}
}

// Another port on localhost is another origin, and a page served from it is
// not the Seedora page.
func TestGuardRefusesAnotherLocalhostPort(t *testing.T) {
	h := newServer(t, usersDDL)

	w := request(t, h, "POST", "/api/seed", map[string]string{
		"Origin": "http://127.0.0.1:3000",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

// What the guard must not break: the page's own fetches, the address bar, and
// the clients that are not browsers at all.
func TestGuardAllowsThePageAndNonBrowsers(t *testing.T) {
	h := newServer(t, usersDDL)

	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"the page's own fetch", map[string]string{
			"Sec-Fetch-Site": "same-origin",
			"Origin":         "http://127.0.0.1:7777",
		}},
		{"typed into the address bar", map[string]string{
			"Sec-Fetch-Site": "none",
		}},
		{"localhost by name", map[string]string{
			"Host":           "localhost:7777",
			"Sec-Fetch-Site": "same-origin",
			"Origin":         "http://localhost:7777",
		}},
		{"IPv6 loopback", map[string]string{
			"Host":           "[::1]:7777",
			"Sec-Fetch-Site": "same-origin",
			"Origin":         "http://[::1]:7777",
		}},
		{"curl, which sends neither header", nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := request(t, h, "GET", "/api/state", c.headers)
			if w.Code != http.StatusOK {
				t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
			}
		})
	}
}

// The page itself, not only the API. A hostile site cannot read it, but it can
// frame it, and a Host check that skipped the document would leave the
// rebinding path open at its first step.
func TestGuardCoversThePage(t *testing.T) {
	h := newServer(t, usersDDL)

	w := request(t, h, "GET", "/", map[string]string{
		"Host":           "rebind.evil.example:7777",
		"Sec-Fetch-Site": "none",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

// Binding somewhere other than loopback is a deliberate act — a container
// publishing the port, a machine on a LAN — and the name it is then reached by
// is the user's to choose, so the Host check steps aside. The cross-site check
// does not.
func TestGuardAllowsAnyHostWhenBoundBeyondLoopback(t *testing.T) {
	h := newServerBoundTo(t, "0.0.0.0", usersDDL)

	w := request(t, h, "GET", "/api/state", map[string]string{
		"Host":           "seedora.internal:7777",
		"Sec-Fetch-Site": "same-origin",
		"Origin":         "http://seedora.internal:7777",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}

	w = request(t, h, "GET", "/api/state", map[string]string{
		"Host":           "seedora.internal:7777",
		"Sec-Fetch-Site": "cross-site",
		"Origin":         "http://evil.example",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-site: got %d, want 403", w.Code)
	}
}
