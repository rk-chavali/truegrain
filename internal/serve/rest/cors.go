package rest

import (
	"net/http"
	"slices"
	"strings"
)

// Cross-origin access, for a console served from somewhere else.
//
// The engine and a browser client are not necessarily on one origin: a
// self-hosted console is a static build that an operator puts wherever suits
// them. Without these headers a browser will make the request and then refuse
// to let the page read the answer, which looks exactly like the engine being
// down.
//
// Three decisions here are security decisions rather than conveniences.
//
// There is no wildcard. `Access-Control-Allow-Origin: *` on an engine running
// without authentication, which is the normal local configuration, would let
// any website the operator happens to visit read their entire model. Only
// origins named at startup are answered.
//
// The Origin header is never reflected unchecked. Echoing whatever the browser
// sent is a wildcard with extra steps, and unlike a wildcard it keeps working
// when credentials are attached.
//
// It is off unless configured. An engine nobody is browsing to has no reason
// to answer a cross-origin request at all.

// CORS allows browser clients from the named origins to read responses.
//
// An origin is a scheme, host and port with no path, for example
// `http://localhost:5180`. Matching is exact: a suffix match would let
// `evil-localhost:5180` through.
type CORS struct {
	origins []string
}

// NewCORS builds the policy. An empty list disables it entirely, which is the
// default and the right setting for a deployment with no browser client.
func NewCORS(origins []string) *CORS {
	cleaned := make([]string, 0, len(origins))
	for _, o := range origins {
		if o = strings.TrimSpace(strings.TrimRight(o, "/")); o != "" {
			cleaned = append(cleaned, o)
		}
	}
	return &CORS{origins: cleaned}
}

// Enabled reports whether any origin is allowed, which health repeats so an
// operator can see it without reading the command line.
func (c *CORS) Enabled() bool { return c != nil && len(c.origins) > 0 }

// Origins lists what is allowed, for health.
func (c *CORS) Origins() []string {
	if c == nil {
		return nil
	}
	return slices.Clone(c.origins)
}

// Wrap adds the headers to responses for allowed origins and answers
// preflight requests.
//
// A request from an origin that is not allowed is served normally without the
// headers, so the browser withholds the response from the page. Refusing the
// request outright would be no more secure and would make a misconfigured
// origin look like an outage rather than a policy.
func (c *CORS) Wrap(next http.Handler) http.Handler {
	if !c.Enabled() {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		// Told to caches unconditionally: the response body is the same for
		// every origin, but these headers are not, and a cache that ignores
		// that will hand one origin's permission to another.
		w.Header().Add("Vary", "Origin")

		if origin != "" && slices.Contains(c.origins, origin) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			// The console sends a bearer token in a header rather than a
			// cookie, so the browser does not need credentialed mode and this
			// stays off. Turning it on would make every allowed origin able to
			// act with the operator's ambient session.
			// User-Agent is here because the clients set one so the audit log
			// can tell one caller from another, and a browser treats it as a
			// header worth asking permission for. Listing it explicitly is
			// better than echoing back whatever the browser requested, which
			// would be an allowlist that allows anything.
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, User-Agent")
			h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			h.Set("Access-Control-Max-Age", "600")
		}

		// A preflight is answered here and never reaches a handler: it carries
		// no credential and asking the engine to plan a query for it would be
		// work done for a request that is not one.
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
