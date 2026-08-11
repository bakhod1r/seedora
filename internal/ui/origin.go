package ui

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// The page holds a live database connection and every mutating route on it —
// connect, apply, seed, import — runs without a password, because the only
// client is meant to be the browser tab Seedora just opened on the user's own
// machine. Nothing about a loopback bind enforces that. Any page the user has
// open can POST to http://127.0.0.1:7777 from its own JavaScript, and a domain
// that resolves to 127.0.0.1 makes the browser treat this server as that
// domain's own origin, which defeats an Origin check on its own.
//
// So there are two questions, and they are different:
//
//   - Is this request coming from the Seedora page, or from some other site in
//     the same browser? Sec-Fetch-Site and Origin answer that.
//   - Is this request addressed to Seedora, or to a domain that happens to
//     point here? The Host header answers that, and only that.
//
// Both are enforced. Requests with neither Sec-Fetch-Site nor Origin are
// allowed: that is curl, a CI script, and the tests, none of which a hostile
// web page can produce — a browser always sends at least one of the two on a
// cross-site request.

// guard rejects requests that a browser on another site could have sent, and
// requests addressed to a hostname that merely resolves here.
//
// bindHost is the address the server was told to listen on. When it is
// loopback — the default, and the only case where a rebound DNS name reaches
// this process — the Host header must be a loopback literal. When the user has
// deliberately bound elsewhere (0.0.0.0 in a container, a LAN address), any
// Host is accepted: they have published the port on purpose and the name it is
// reached by is theirs to choose.
func guard(bindHost string, h http.Handler) http.Handler {
	checkHost := isLoopbackHost(bindHost)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := checkSite(r); err != nil {
			writeErr(w, http.StatusForbidden, err)
			return
		}
		if checkHost {
			if err := checkLoopbackHost(r.Host); err != nil {
				writeErr(w, http.StatusForbidden, err)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// checkSite reports whether the request came from Seedora's own page.
//
// Sec-Fetch-Site is the reliable signal and the browser sets it, not the
// script: same-origin is the page's own fetch, none is the address bar. cross-
// site and same-site are another page, including another port on localhost,
// which is another origin and not this one.
func checkSite(r *http.Request) error {
	switch site := r.Header.Get("Sec-Fetch-Site"); site {
	case "same-origin", "none":
		return nil
	case "":
		// No Sec-Fetch-Site: either a non-browser client, or a browser old
		// enough to predate the header. Fall through to Origin, which such a
		// browser does still send on a cross-origin request.
	default:
		return fmt.Errorf("refusing a %s request: the Seedora API is reachable "+
			"only from the page Seedora serves", site)
	}

	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil {
		return fmt.Errorf("refusing a request with an unparseable Origin")
	}
	// Same origin means same host and port. The scheme is always http here:
	// this server does not speak TLS, so a request that reached it over https
	// went through something else, and that something else is not the page.
	if u.Host != r.Host {
		return fmt.Errorf("refusing a cross-origin request from %s: the Seedora "+
			"API is reachable only from the page Seedora serves", origin)
	}
	return nil
}

// checkLoopbackHost rejects a Host header that is not a loopback literal. This
// is the DNS-rebinding defence: a hostile name resolved to 127.0.0.1 arrives
// here with its own name in Host, and the browser considers it same-origin, so
// no other header in the request can tell the two apart.
func checkLoopbackHost(host string) error {
	if isLoopbackHost(host) {
		return nil
	}
	return fmt.Errorf("refusing a request addressed to %q: Seedora is bound to "+
		"loopback and answers only to localhost, 127.0.0.1, or [::1]", host)
}

// isLoopbackHost reports whether a host — with or without a port — names the
// local machine by literal address or by the one name that cannot be pointed
// anywhere else.
//
// "localhost" is accepted as a name rather than resolved. It is reserved to
// loopback by RFC 6761, and resolving it would make the check depend on the
// machine's resolver, which is exactly the thing rebinding attacks control.
// Every other name is refused, including one that resolves here today.
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
