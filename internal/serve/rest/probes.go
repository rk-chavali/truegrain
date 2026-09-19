package rest

import (
	"net/http"
)

// Liveness and readiness, for an orchestrator rather than for a caller.
//
// Three endpoints report health here and they answer three different
// questions, which is why they are not one.
//
//	GET /healthz   is this process alive, should it be restarted
//	GET /readyz    should traffic be sent here right now
//	GET /v1/health what does this deployment enforce, for a human or a console
//
// Deliberately outside /v1/. The versioned surface is the client contract, is
// published as api/openapi.yaml and is checked against the mux in both
// directions; a probe is operational plumbing that no SDK should grow a method
// for. /healthz and /readyz are also the names every Kubernetes operator
// already greps for.
//
// Neither probe touches the warehouse. That is the whole reason they exist
// separately from /v1/health.
//
// A liveness probe that failed on a warehouse outage would have Kubernetes
// restart every replica during someone else's incident, and restarting cannot
// fix a warehouse. A readiness probe that failed on it would pull every
// replica out of the load balancer at once, turning a partial outage into a
// total one: with the warehouse down this engine still answers /v1/metrics,
// /v1/compile and every metadata route, and still refuses a fan-out. Taking it
// out of service would withdraw the half that still works.
//
// Neither is authenticated. A kubelet does not carry a bearer token, and
// making the probe the one route that needs credentials is how deployments end
// up with no probes at all. They are safe to leave open because they disclose
// nothing: liveness is a constant, and readiness says only whether a model is
// loaded, never which one or what is in it.

// unversioned are the routes served outside /v1/, kept apart from
// handlers so the typed client contract and everything else cannot be
// confused for each other. TestUnversionedRoutesAreNotInTheContract
// asserts the separation holds.
//
// Two kinds live here and neither belongs in the contract.
//
// The probes answer an orchestrator, and an SDK growing a live() method
// would be noise.
//
// GraphQL is a whole other protocol with its own request envelope and its
// own error convention. It is for a browser client that already speaks
// it; a Python, Go or TypeScript caller uses query(), and making all
// three grow a graphql() method that posts a string would be worse than
// useless. It serves the same vocabulary, so a refusal means exactly what
// it means over REST.
func (s *Server) unversioned() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /healthz":  s.live,
		"GET /readyz":   s.ready,
		"POST /graphql": s.graphQL,
	}
}

// live answers whether the process should be restarted.
//
// It is a constant, and that is correct rather than lazy. The only thing a
// restart fixes is a process that cannot serve HTTP, and a handler that
// returns at all has just proved it can. Every richer check that has ever been
// put behind a liveness probe, a database ping, a disk check, a dependency
// fetch, converts somebody else's outage into a restart loop of your own.
func (s *Server) live(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// ready answers whether this replica should receive traffic.
//
// One condition: a model is loaded and has a namespace that can answer. That
// is the entire difference between a process that is up and a process that is
// useful, and it is the window this exists for. A reload that fails leaves the
// previous model serving, so this keeps reporting ready, which is right: the
// replica is still answering correctly from the last good model.
func (s *Server) ready(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	eng := s.engine()
	if eng == nil {
		// Before the first model is installed. A replica that has not loaded
		// anything would answer every metadata request with an error, so it
		// must not be in the load balancer yet.
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("no model is loaded\n"))
		return
	}
	if len(eng.Workspace().Available()) == 0 {
		// Every namespace failed to load. The process is alive and will pick
		// up a fix on the next reload, so this is not a restart; it is a
		// replica that cannot answer anything yet.
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("a model is loaded but no namespace is usable\n"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}
