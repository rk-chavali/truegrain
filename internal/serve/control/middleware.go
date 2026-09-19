package control

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rk-chavali/truegrain/internal/control"
	"github.com/rk-chavali/truegrain/internal/govern"
)

// Session cookies, CSRF, rate limiting and the headers a browser needs.
//
// The control plane is the first surface in this engine a browser talks to
// directly, which means it is the first one exposed to the attacks a
// browser enables: a cookie sent automatically on a cross-site request, a
// script injected into a page, a login form hammered from a botnet. None of
// those apply to a bearer token on an API call, which is why none of this
// exists in internal/serve/rest.

// SessionCookie is the cookie name.
//
// The __Host- prefix is a browser-enforced guarantee: a cookie named this
// way must be Secure, must have Path=/, and must have no Domain, which
// means a subdomain cannot set or overwrite it. It costs nothing and closes
// subdomain cookie fixation. It requires HTTPS, so plain HTTP falls back to
// an ordinary name; see cookieName.
const (
	secureCookieName = "__Host-truegrain_session"
	plainCookieName  = "truegrain_session"
)

// cookieName picks the name that matches the scheme.
//
// A __Host- cookie over plain HTTP is silently dropped by the browser,
// which would make local development look like a broken login rather than
// a cookie policy. So the prefix is used exactly when it can work.
func cookieName(secure bool) string {
	if secure {
		return secureCookieName
	}
	return plainCookieName
}

// setSession writes the session cookie.
func (s *Server) setSession(w http.ResponseWriter, r *http.Request, token string) {
	secure := isTLS(r)
	http.SetCookie(w, &http.Cookie{
		Name:  cookieName(secure),
		Value: token,
		Path:  "/",
		// Not readable from JavaScript. An XSS that gets into the page still
		// cannot exfiltrate the session, which turns a total compromise into
		// a bad afternoon.
		HttpOnly: true,
		Secure:   secure,
		// Lax rather than Strict. Strict would log somebody out every time
		// they followed a link into the console from Slack, which is how a
		// team actually arrives at it. Lax still withholds the cookie from
		// cross-site POSTs, which is the case CSRF needs.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(control.SessionMaximum / time.Second),
	})
}

// clearSession removes it, on logout.
func (s *Server) clearSession(w http.ResponseWriter, r *http.Request) {
	secure := isTLS(r)
	http.SetCookie(w, &http.Cookie{
		Name: cookieName(secure), Value: "", Path: "/",
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

func (s *Server) sessionToken(r *http.Request) string {
	for _, name := range []string{secureCookieName, plainCookieName} {
		if c, err := r.Cookie(name); err == nil && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

// isTLS reports whether the browser reached this over HTTPS.
//
// X-Forwarded-Proto is honoured only when the operator said a proxy is in
// front, because a client can send that header itself. Trusting it by
// default would let anyone claim their plaintext request was secure and
// have the Secure cookie set on a connection that is not.
func isTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if trustForwardedProto.Load() {
		return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	}
	return false
}

// trustForwardedProto is set by the server when -behind-proxy is given.
var trustForwardedProto atomicBool

// user is the authenticated caller, or nil.
type ctxKey struct{}

func withUser(ctx context.Context, u *control.User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// userOf returns the caller, or nil when the request is not authenticated.
func userOf(r *http.Request) *control.User {
	u, _ := r.Context().Value(ctxKey{}).(*control.User)
	return u
}

// RoleGroupPrefix marks the group that carries an account's role.
//
// Reserved: a group beginning with this never came from an invitation or
// from the People screen, because identityOf strips those before adding the
// real one. Anything matching it is the account's actual role.
const RoleGroupPrefix = "role:"

// identityOf turns a control plane account into the identity the governance
// gate enforces against.
//
// This function is the entire reason the control plane is worth building.
// Before it, a policy could only name a service account, because a bearer
// token was the only identity the engine ever saw. Now a policy written
// against `analysts` applies to the person who logged in, and the audit log
// records their email instead of a shared token's subject.
//
// The identity is derived per request and passed down, never stored as a
// "current user". That rule is why the engine can serve a hundred callers
// at once and still resolve policy for each of them.
func identityOf(u *control.User) govern.Identity {
	if u == nil {
		return govern.Identity{}
	}
	/*
		The account's role travels as a reserved group, so that the engine
		can name it without learning what a control plane role is. The
		engine matches subjects and groups; it has no concept of owner or
		member and should not grow one.

		Filtering the user's own groups first is the security property here
		rather than tidiness. Groups are typed by an admin on the People
		screen, so without this an admin could give a member a group
		literally called "role:admin" and hand them the audit log while
		the interface went on showing them as a member. Reserving the
		prefix makes the string unforgeable: it can only come from the
		role the account actually holds.
	*/
	groups := make([]string, 0, len(u.Groups)+1)
	for _, group := range u.Groups {
		if !strings.HasPrefix(group, RoleGroupPrefix) {
			groups = append(groups, group)
		}
	}
	groups = append(groups, RoleGroupPrefix+string(u.Role))

	return govern.Identity{
		Subject: u.Email,
		Groups:  groups,
		TokenID: u.ID,
	}
}

// authenticate resolves the session cookie and attaches the user.
//
// It never rejects. Routes decide what they need: the bootstrap endpoint
// answers whether the instance is claimed without a session, and the login
// page has to render for somebody who has none.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := s.sessionToken(r)
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		u, err := s.store.Session(r.Context(), token)
		if err != nil {
			// An expired or revoked session. Clear the cookie so the browser
			// stops sending it and the user sees a login page rather than a
			// silently half-working app.
			s.clearSession(w, r)
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(withUser(r.Context(), u)))
	})
}

// requireUser refuses anybody not logged in.
func (s *Server) requireUser(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if userOf(r) == nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in to continue")
			return
		}
		h(w, r)
	}
}

// requireRole refuses anybody below a role.
func (s *Server) requireRole(role control.Role, h http.HandlerFunc) http.HandlerFunc {
	return s.requireUser(func(w http.ResponseWriter, r *http.Request) {
		if u := userOf(r); !u.Role.AtLeast(role) {
			// Says what is needed rather than only that it was refused,
			// because the caller is a logged-in colleague and "ask an admin"
			// is the actionable version.
			writeError(w, http.StatusForbidden, "forbidden",
				"this needs the "+string(role)+" role or higher")
			return
		}
		h(w, r)
	})
}

// CSRFHeader is the header a state-changing request must carry.
//
// The defence is that a browser will not send a custom header on a
// cross-site request without a CORS preflight, and this server allows no
// cross-origin request at all. A form POST from evil.example.com carries
// the cookie but cannot add this, so it is rejected before it does
// anything.
//
// Chosen over a synchroniser token because there is no token to leak, no
// per-session state to store, and nothing to get wrong when the page is
// served from a cache. SameSite=Lax is the second layer, not the only one:
// it is a browser default that has been relaxed before.
const CSRFHeader = "X-Truegrain-Console"

// requireCSRF refuses a state-changing request that a form could have sent.
func requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get(CSRFHeader) == "" {
			writeError(w, http.StatusForbidden, "csrf",
				"this request is missing the "+CSRFHeader+" header, which every "+
					"console request carries and a cross-site form cannot add")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders sets what a browser needs to be told.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// The console is one origin serving its own scripts and styles and
		// talking only to itself. Everything else is denied, so an injected
		// <script src="evil"> has nowhere to load from and an exfiltration
		// attempt has nowhere to send to.
		//
		// 'unsafe-inline' on style-src only: the bundler emits a stylesheet
		// but React still sets inline styles for measured layout, and a
		// nonce pipeline for that is not worth it while script-src stays
		// strict. Script injection is the one that steals sessions.
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; font-src 'self' data:; connect-src 'self'; "+
				"frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		// Redundant with frame-ancestors for modern browsers and free.
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// limiter is a fixed-window rate limiter keyed by a string.
//
// Deliberately crude. A token bucket would be smoother and this is
// protecting a login form on a self-hosted instance, where the realistic
// attacker is a script trying a password list rather than a distributed
// botnet shaping its traffic. What matters is that the tenth attempt in a
// minute fails, and this does that in forty lines.
//
// ponytail: fixed window, in memory, per process. Two replicas mean twice
// the attempts, and a window boundary allows a burst of 2n. Move to the
// shared Postgres if either matters.
type limiter struct {
	mu      sync.Mutex
	hits    map[string]*window
	limit   int
	window  time.Duration
	lastGC  time.Time
	maxKeys int
}

type window struct {
	count int
	start time.Time
}

func newLimiter(limit int, per time.Duration) *limiter {
	return &limiter{
		hits: map[string]*window{}, limit: limit, window: per,
		lastGC: time.Now(), maxKeys: 10000,
	}
}

// allow reports whether this key may proceed.
func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	l.collect(now)

	w, ok := l.hits[key]
	if !ok || now.Sub(w.start) > l.window {
		l.hits[key] = &window{count: 1, start: now}
		return true
	}
	w.count++
	return w.count <= l.limit
}

// collect drops expired windows, so an instance under a spray attack does
// not accumulate a map entry per source address forever.
func (l *limiter) collect(now time.Time) {
	if now.Sub(l.lastGC) < l.window && len(l.hits) < l.maxKeys {
		return
	}
	for k, w := range l.hits {
		if now.Sub(w.start) > l.window {
			delete(l.hits, k)
		}
	}
	// A spray from more distinct addresses than the cap can hold is a
	// memory problem, and dropping the whole table is the safe response:
	// it forgets some in-progress limits rather than growing without
	// bound, and the attacker gains one window.
	if len(l.hits) >= l.maxKeys {
		clear(l.hits)
	}
	l.lastGC = now
}

// clientIP is the best available source address.
//
// X-Forwarded-For is honoured only behind a declared proxy, for the same
// reason as X-Forwarded-Proto: otherwise an attacker sets it per request
// and every attempt looks like a different client, which turns the rate
// limiter off.
func clientIP(r *http.Request) string {
	if trustForwardedProto.Load() {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			first, _, _ := strings.Cut(fwd, ",")
			if ip := strings.TrimSpace(first); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// logAttempt records an authentication outcome.
//
// The email is recorded because an operator investigating a break-in needs
// to know which account was targeted. The password never is, not even its
// length, and not even on success.
func (s *Server) logAttempt(r *http.Request, event, email string, err error) {
	attrs := []any{
		slog.String("event", event),
		slog.String("email", control.NormaliseEmail(email)),
		slog.String("ip", clientIP(r)),
	}
	if err != nil {
		s.log.Warn("authentication failed", append(attrs, slog.String("reason", err.Error()))...)
		return
	}
	s.log.Info("authenticated", attrs...)
}

// atomicBool is a tiny wrapper so the proxy flag reads cleanly.
type atomicBool struct{ v sync.Map }

func (a *atomicBool) Store(b bool) { a.v.Store("v", b) }
func (a *atomicBool) Load() bool {
	v, ok := a.v.Load("v")
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}
