package control

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rk-chavali/truegrain/internal/console"
	"github.com/rk-chavali/truegrain/internal/control"
	"github.com/rk-chavali/truegrain/internal/govern"
)

// The control plane's HTTP surface.
//
// Accounts, sessions, people and warehouse connections, plus serving the
// console that drives them. It sits in front of the engine's own REST API
// rather than replacing it: the console calls /v1/metrics and /v1/query
// exactly as any other client does, and the session cookie resolves to the
// govern.Identity those routes already enforce against.
//
// That last part is the whole design. There is no second query path, no
// second governance check, and no way for the console to reach data the
// REST API would refuse. Adding a login did not add an authorisation
// system; it added a way for the existing one to know who a person is.
//
// Everything here is /api/. The engine keeps /v1/, the probes keep
// /healthz and /readyz, and the console is served from /. Three prefixes
// that cannot collide, so a route added to one cannot shadow another.

// consoleBuilt records whether a real UI was embedded, so the SPA
// handler can serve the explanatory page instead of an empty directory.
var consoleBuilt = console.Built()

// Server is the control plane.
type Server struct {
	store *control.Store
	log   *slog.Logger

	// engine is the REST handler this wraps, or nil when the control plane
	// runs without a model loaded. Nil is a real state: an operator brings
	// the stack up, signs in, and connects a warehouse before there is
	// anything to query.
	engine http.Handler

	// console is the built UI, embedded. Nil serves a message saying so,
	// which is what a development build does while Vite serves the UI on
	// its own port.
	console fs.FS

	loginLimit *limiter
	ipLimit    *limiter
}

// Options configure the server.
type Options struct {
	Store   *control.Store
	Logger  *slog.Logger
	Engine  http.Handler
	Console fs.FS
	// BehindProxy makes X-Forwarded-Proto and X-Forwarded-For trusted.
	// Off by default because a client can send both, and trusting them
	// unconditionally turns off the rate limiter and lets a plaintext
	// request claim to be secure.
	BehindProxy bool
}

// New builds the control plane.
func New(opts Options) *Server {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	trustForwardedProto.Store(opts.BehindProxy)
	return &Server{
		store:   opts.Store,
		log:     log,
		engine:  opts.Engine,
		console: opts.Console,
		// Ten failed logins a minute per account, and thirty per address.
		// Per account so a password list against one person is slowed;
		// per address so a spray across many accounts from one place is
		// too. Either alone leaves the other open.
		loginLimit: newLimiter(10, time.Minute),
		ipLimit:    newLimiter(30, time.Minute),
	}
}

// Handler returns the routed server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated: what the UI needs before anybody has signed in.
	mux.HandleFunc("GET /api/bootstrap", s.bootstrap)
	mux.HandleFunc("POST /api/auth/claim", s.claim)
	mux.HandleFunc("POST /api/auth/login", s.login)
	mux.HandleFunc("POST /api/auth/logout", s.logout)
	mux.HandleFunc("GET /api/invites/{token}", s.peekInvite)
	mux.HandleFunc("POST /api/invites/{token}/accept", s.acceptInvite)

	// Signed in.
	mux.HandleFunc("GET /api/me", s.requireUser(s.me))
	mux.HandleFunc("POST /api/me/password", s.requireUser(s.changePassword))

	// Administration.
	mux.HandleFunc("GET /api/people", s.requireRole(control.RoleAdmin, s.people))
	mux.HandleFunc("POST /api/people/invite", s.requireRole(control.RoleAdmin, s.invite))
	mux.HandleFunc("PATCH /api/people/{id}", s.requireRole(control.RoleAdmin, s.updatePerson))
	mux.HandleFunc("DELETE /api/people/invite/{email}", s.requireRole(control.RoleAdmin, s.revokeInvite))

	mux.HandleFunc("GET /api/events", s.requireRole(control.RoleAdmin, s.events))
	mux.HandleFunc("GET /api/connections", s.requireRole(control.RoleAdmin, s.connections))
	mux.HandleFunc("POST /api/connections", s.requireRole(control.RoleAdmin, s.createConnection))
	mux.HandleFunc("DELETE /api/connections/{name}", s.requireRole(control.RoleAdmin, s.deleteConnection))

	// The engine, under its own prefix, reached with the session cookie.
	if s.engine != nil {
		mux.Handle("/v1/", s.engine)
		mux.Handle("/healthz", s.engine)
		mux.Handle("/readyz", s.engine)
	}

	// The console, and every unmatched path, because a single page app
	// owns its own routing and a refresh on /people must not 404.
	mux.HandleFunc("/", s.serveConsole)

	return securityHeaders(requireCSRF(s.authenticate(mux)))
}

// Authenticator adapts the session cookie to the engine's REST
// authenticator, so /v1/ routes see the logged-in person.
//
// This is what makes a policy written against `analysts` apply to a human.
// The engine is unchanged: it asks its Authenticator who is calling and
// gets back a govern.Identity, exactly as it does for a bearer token.
type Authenticator struct{ Store *control.Store }

// Authenticate resolves the session cookie.
func (a Authenticator) Authenticate(r *http.Request) (govern.Identity, error) {
	// The middleware has usually resolved this already, so the common path
	// is free.
	if u := userOf(r); u != nil {
		return identityOf(u), nil
	}
	for _, name := range []string{secureCookieName, plainCookieName} {
		c, err := r.Cookie(name)
		if err != nil || c.Value == "" {
			continue
		}
		u, err := a.Store.Session(r.Context(), c.Value)
		if err == nil {
			return identityOf(u), nil
		}
	}
	return govern.Identity{}, errors.New("sign in to continue")
}

// Name reports what this authenticates with, for /v1/health.
func (a Authenticator) Name() string { return "console session" }

// bootstrap is the first call the UI makes.
//
// It answers three questions in one round trip: has anybody claimed this
// instance, who am I, and is there a model loaded. The UI needs all three
// to decide whether to render a setup form, a login form or the app, and
// three separate requests would each render a different wrong thing first.
func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	claimed, err := s.store.Claimed(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	body := map[string]any{
		"claimed":     claimed,
		"has_engine":  s.engine != nil,
		"csrf_header": CSRFHeader,
	}
	if org, err := s.store.Org(r.Context()); err == nil {
		body["org"] = org
	}
	if u := userOf(r); u != nil {
		body["user"] = u
	}
	writeJSON(w, http.StatusOK, body)
}

type claimRequest struct {
	Org      string `json:"org"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

// claim creates the organisation and its first owner.
//
// Open exactly once. After the first account exists this refuses, so the
// window in which anybody who can reach the port becomes an owner is the
// seconds between the container starting and the operator using it. That
// is the same model Metabase and Grafana use, and it is the only one that
// works without a shared secret to bootstrap with.
//
// The check and the insert race in principle: two requests could both see
// an unclaimed instance. The loser fails on the unique email constraint if
// they used the same address, and otherwise creates a second owner, which
// is why the log records it loudly rather than pretending it cannot
// happen.
func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	if !s.ipLimit.allow("claim:" + clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many attempts")
		return
	}

	var req claimRequest
	if !decode(w, r, &req) {
		return
	}

	claimed, err := s.store.Claimed(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	if claimed {
		// Deliberately not "an owner already exists", which would confirm
		// to an unauthenticated caller that the instance is in use.
		writeError(w, http.StatusConflict, "already_claimed",
			"this instance has already been set up. Sign in, or ask an "+
				"administrator for an invitation")
		return
	}

	orgName := strings.TrimSpace(req.Org)
	if orgName == "" {
		orgName = "My organisation"
	}
	org, err := s.store.CreateOrg(r.Context(), orgName)
	if err != nil {
		s.fail(w, err)
		return
	}
	u, err := s.store.CreateUser(r.Context(), org.ID, req.Email, req.Name, req.Password,
		control.RoleOwner, nil)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	s.log.Info("instance claimed",
		slog.String("org", org.Name), slog.String("owner", u.Email),
		slog.String("ip", clientIP(r)))

	s.startSession(w, r, u)
	writeJSON(w, http.StatusCreated, map[string]any{"user": u, "org": org})
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decode(w, r, &req) {
		return
	}

	// Two limiters. Per account, so a password list against one person is
	// slowed even from many addresses; per address, so a spray across many
	// accounts from one place is slowed too.
	email := control.NormaliseEmail(req.Email)
	if !s.loginLimit.allow("login:"+email) || !s.ipLimit.allow("login:"+clientIP(r)) {
		s.logAttempt(r, "login.rate_limited", email, errors.New("too many attempts"))
		writeError(w, http.StatusTooManyRequests, "rate_limited",
			"too many sign-in attempts. Wait a minute and try again")
		return
	}

	u, err := s.store.Authenticate(r.Context(), req.Email, req.Password)
	if err != nil {
		s.logAttempt(r, "login.failed", email, err)
		// Not recorded in control_events. A failed sign-in has no org to
		// attribute it to: the address may belong to nobody, and inserting a
		// row keyed on a guessed org would let an unauthenticated caller write
		// to the audit table by typing an email. The rate limiter above is
		// what answers a password list, and logAttempt keeps the trail.
		writeError(w, http.StatusUnauthorized, "bad_credentials", control.ErrBadCredentials.Error())
		return
	}
	s.logAttempt(r, "login.ok", email, nil)
	// A successful one does have an org, and "who has been signing in" is half
	// of any access review.
	s.store.Record(r.Context(), u.OrgID, u.ID, u.Email, control.ActionSignedIn,
		"", control.Allowed, clientIP(r))
	s.startSession(w, r, u)
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

// startSession mints a fresh session and sets the cookie.
//
// Always a new token. Reusing one across a login is session fixation.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u *control.User) {
	token, err := s.store.CreateSession(r.Context(), u.ID,
		r.Header.Get("User-Agent"), clientIP(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.setSession(w, r, token)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if token := s.sessionToken(r); token != "" {
		// Server side, so the session ends rather than the browser merely
		// forgetting it. The reason somebody logs out is often that the
		// machine is not theirs.
		if err := s.store.EndSession(r.Context(), token); err != nil {
			s.log.Warn("ending a session failed", slog.String("error", err.Error()))
		}
	}
	s.clearSession(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"user": userOf(r)})
}

type passwordRequest struct {
	Current string `json:"current"`
	New     string `json:"new"`
}

// changePassword requires the current one.
//
// Not a formality: without it, an XSS or a borrowed laptop turns into
// permanent account takeover in one request. Requiring the current
// password means the attacker needs the thing they were trying to get.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var req passwordRequest
	if !decode(w, r, &req) {
		return
	}
	u := userOf(r)
	if _, err := s.store.Authenticate(r.Context(), u.Email, req.Current); err != nil {
		s.logAttempt(r, "password.reauth_failed", u.Email, err)
		writeError(w, http.StatusUnauthorized, "bad_credentials",
			"that is not your current password")
		return
	}
	if err := s.store.SetPassword(r.Context(), u.ID, req.New); err != nil {
		s.badRequest(w, err)
		return
	}
	// SetPassword ends every session, including this one, so the browser
	// is signed out and has to sign in with the new password. That is the
	// correct outcome and the UI says so rather than appearing to break.
	s.clearSession(w, r)
	s.log.Info("password changed", slog.String("user", u.Email))
	writeJSON(w, http.StatusOK, map[string]any{"signed_out": true})
}

func (s *Server) people(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	users, err := s.store.Users(r.Context(), u.OrgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	invites, err := s.store.Invites(r.Context(), u.OrgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users, "invites": invites})
}

type inviteRequest struct {
	Email  string   `json:"email"`
	Role   string   `json:"role"`
	Groups []string `json:"groups"`
}

// invite creates an invitation and returns the link exactly once.
//
// There is no mail sender here and that is deliberate for a first version:
// configuring SMTP is the single most common reason a self-hosted install
// stalls, and a link an admin pastes into Slack works on every network
// including one with no outbound mail at all.
func (s *Server) invite(w http.ResponseWriter, r *http.Request) {
	var req inviteRequest
	if !decode(w, r, &req) {
		return
	}
	u := userOf(r)
	role := control.Role(req.Role)
	if role == "" {
		role = control.RoleMember
	}
	// An admin cannot invite somebody more powerful than themselves.
	if !u.Role.AtLeast(role) {
		// Recorded, not just refused. Somebody trying to invite above their
		// own role is the row an access review most wants to see, and a
		// refusal that leaves no trace is indistinguishable from never having
		// been attempted.
		s.store.Record(r.Context(), u.OrgID, u.ID, u.Email, control.ActionInvited,
			control.NormaliseEmail(req.Email), control.Refused, "attempted at "+string(role))
		writeError(w, http.StatusForbidden, "forbidden",
			"you cannot invite somebody at a role above your own")
		return
	}

	token, err := s.store.CreateInvite(r.Context(), u.OrgID, req.Email, role, req.Groups, u.ID)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	s.log.Info("invitation created",
		slog.String("email", control.NormaliseEmail(req.Email)),
		slog.String("role", string(role)),
		slog.String("by", u.Email))
	s.store.Record(r.Context(), u.OrgID, u.ID, u.Email, control.ActionInvited,
		control.NormaliseEmail(req.Email), control.Allowed, "as "+string(role))

	writeJSON(w, http.StatusCreated, map[string]any{
		"email": control.NormaliseEmail(req.Email),
		"role":  role,
		// The path, not a full URL. The server does not reliably know its
		// own external address behind a proxy, and a link built from a
		// guessed host is a link that does not work.
		"accept_path": "/invite/" + token,
		"expires_in":  control.InviteLifetime.String(),
		"note": "This link is shown once and is not stored. Send it to them; " +
			"if it is lost, issue a new invitation.",
	})
}

func (s *Server) peekInvite(w http.ResponseWriter, r *http.Request) {
	if !s.ipLimit.allow("invite:" + clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many attempts")
		return
	}
	i, err := s.store.PeekInvite(r.Context(), r.PathValue("token"))
	if err != nil {
		writeError(w, http.StatusNotFound, "invalid_invite",
			"that invitation is not valid. It may have been used already, or expired")
		return
	}
	// Only what the holder already knows. Not who sent it, and nothing
	// about anybody else: this is reachable without signing in.
	writeJSON(w, http.StatusOK, map[string]any{
		"email": i.Email, "role": i.Role, "expires_at": i.ExpiresAt,
	})
}

type acceptRequest struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

func (s *Server) acceptInvite(w http.ResponseWriter, r *http.Request) {
	if !s.ipLimit.allow("accept:" + clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many attempts")
		return
	}
	var req acceptRequest
	if !decode(w, r, &req) {
		return
	}
	u, err := s.store.AcceptInvite(r.Context(), r.PathValue("token"), req.Name, req.Password)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	s.log.Info("invitation accepted", slog.String("user", u.Email),
		slog.String("role", string(u.Role)))
	s.startSession(w, r, u)
	writeJSON(w, http.StatusCreated, map[string]any{"user": u})
}

type updatePersonRequest struct {
	Role     string   `json:"role"`
	Groups   []string `json:"groups"`
	Disabled bool     `json:"disabled"`
}

// updatePerson changes a member's role, groups or disabled flag.
//
// Two refusals here, and both exist because the alternative is an instance
// nobody can administer: the last owner cannot be demoted or disabled, and
// nobody can promote somebody above their own role.
func (s *Server) updatePerson(w http.ResponseWriter, r *http.Request) {
	var req updatePersonRequest
	if !decode(w, r, &req) {
		return
	}
	actor := userOf(r)
	target, err := s.store.User(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such person")
		return
	}
	if target.OrgID != actor.OrgID {
		writeError(w, http.StatusNotFound, "not_found", "no such person")
		return
	}

	role := control.Role(req.Role)
	if !role.Valid() {
		s.badRequest(w, errors.New("that is not a role"))
		return
	}
	if !actor.Role.AtLeast(role) || !actor.Role.AtLeast(target.Role) {
		// The privilege escalation attempt, recorded. This is the row an
		// access review exists to find, and a refusal that leaves no trace
		// reads the same as one that never happened.
		s.store.Record(r.Context(), actor.OrgID, actor.ID, actor.Email,
			control.ActionRoleChanged, target.Email, control.Refused,
			"attempted "+string(target.Role)+" to "+string(role))
		writeError(w, http.StatusForbidden, "forbidden",
			"you cannot change somebody at or above your own role")
		return
	}

	// The guard that stops an instance from becoming unadministrable.
	losingOwner := target.Role == control.RoleOwner && (role != control.RoleOwner || req.Disabled)
	if losingOwner {
		owners, err := s.store.CountOwners(r.Context(), actor.OrgID)
		if err != nil {
			s.fail(w, err)
			return
		}
		if owners <= 1 {
			writeError(w, http.StatusConflict, "last_owner",
				"this is the last owner. Make somebody else an owner first, or "+
					"there will be nobody who can administer this instance")
			return
		}
	}

	if err := s.store.UpdateUser(r.Context(), target.ID, role, req.Groups, req.Disabled); err != nil {
		s.fail(w, err)
		return
	}
	detail := string(target.Role) + " to " + string(role)
	if req.Disabled {
		detail += ", disabled"
	}
	s.store.Record(r.Context(), actor.OrgID, actor.ID, actor.Email,
		control.ActionRoleChanged, target.Email, control.Allowed, detail)
	if req.Disabled {
		// An open tab is a working session. Disabling somebody who has
		// left and leaving their browser signed in is the case this
		// exists for.
		if err := s.store.EndAllSessions(r.Context(), target.ID); err != nil {
			s.log.Warn("ending sessions failed", slog.String("error", err.Error()))
		}
	}
	s.log.Info("person updated", slog.String("target", target.Email),
		slog.String("role", string(role)), slog.Bool("disabled", req.Disabled),
		slog.String("by", actor.Email))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) revokeInvite(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	if err := s.store.RevokeInvite(r.Context(), u.OrgID, r.PathValue("email")); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such invitation")
		return
	}
	s.store.Record(r.Context(), u.OrgID, u.ID, u.Email, control.ActionInviteRevoked,
		control.NormaliseEmail(r.PathValue("email")), control.Allowed, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) connections(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	list, err := s.store.Connections(r.Context(), u.OrgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connections": list})
}

type connectionRequest struct {
	Name    string            `json:"name"`
	Dialect string            `json:"dialect"`
	Secret  string            `json:"secret"`
	Detail  map[string]string `json:"detail"`
}

// createConnection stores a warehouse and its sealed credential.
//
// The SSRF surface, named rather than hidden: an admin supplies a host and
// this process will connect to it, including to an address inside the
// network it is running in. That is the feature, it cannot be closed
// without removing the feature, and the mitigations are that it takes an
// admin and that every attempt is logged with who made it.
func (s *Server) createConnection(w http.ResponseWriter, r *http.Request) {
	var req connectionRequest
	if !decode(w, r, &req) {
		return
	}
	u := userOf(r)
	c, err := s.store.CreateConnection(r.Context(), u.OrgID, req.Name, req.Dialect,
		req.Secret, req.Detail, u.ID)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	// The name, the dialect and who did it. Never the secret, and never
	// the detail map, which can carry a host somebody treats as sensitive.
	s.log.Info("warehouse connection saved",
		slog.String("name", c.Name), slog.String("dialect", c.Dialect),
		slog.String("by", u.Email), slog.String("ip", clientIP(r)))
	// The dialect only. Not the secret and not the detail map, which carries
	// a host somebody may treat as sensitive, for the same reason the log
	// line above omits both.
	s.store.Record(r.Context(), u.OrgID, u.ID, u.Email, control.ActionConnectionAdded,
		c.Name, control.Allowed, c.Dialect)
	writeJSON(w, http.StatusCreated, map[string]any{"connection": c})
}

func (s *Server) deleteConnection(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	if err := s.store.DeleteConnection(r.Context(), u.OrgID, r.PathValue("name")); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such connection")
		return
	}
	s.log.Info("warehouse connection deleted",
		slog.String("name", r.PathValue("name")), slog.String("by", u.Email))
	s.store.Record(r.Context(), u.OrgID, u.ID, u.Email, control.ActionConnectionRemoved,
		r.PathValue("name"), control.Allowed, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// serveConsole serves the embedded single page app.
//
// Any unmatched path falls back to index.html, because the console owns
// its own routing and a refresh on /people has to work. The fallback is
// deliberately not applied to /api/, /v1/ or an asset request: returning
// HTML for a missing API route would make a typo look like a working
// endpoint returning nonsense.
func (s *Server) serveConsole(w http.ResponseWriter, r *http.Request) {
	if s.console == nil || !consoleBuilt {
		// A page that explains itself, not a 404: a blank response here
		// reads as a routing bug and sends somebody into the server code
		// looking for a problem that is not there.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(console.Placeholder))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}

	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" || name == "." {
		name = "index.html"
	}
	f, err := s.console.Open(name)
	if err != nil {
		// A path with a file extension that does not exist is a broken
		// asset reference, not a client-side route. Saying so turns a
		// blank page into a readable 404.
		if path.Ext(name) != "" {
			writeError(w, http.StatusNotFound, "not_found", "no such file")
			return
		}
		name = "index.html"
		if f, err = s.console.Open(name); err != nil {
			writeError(w, http.StatusNotFound, "not_found", "the console is not built")
			return
		}
	}
	defer f.Close()

	// index.html is never cached, so a deploy is visible on refresh. Hashed
	// assets are cached hard, because their name changes when they do.
	if name == "index.html" {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}

	content, ok := f.(io.ReadSeeker)
	if !ok {
		body, err := io.ReadAll(f)
		if err != nil {
			s.fail(w, err)
			return
		}
		w.Header().Set("Content-Type", contentType(name))
		_, _ = w.Write(body)
		return
	}
	info, err := f.Stat()
	if err != nil {
		s.fail(w, err)
		return
	}
	http.ServeContent(w, r, name, info.ModTime(), content)
}

func contentType(name string) string {
	switch path.Ext(name) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json"
	}
	return "application/octet-stream"
}

// decode reads a JSON body, bounded.
//
// One megabyte. Nothing this API accepts is close to that, and an unbounded
// reader on an unauthenticated endpoint is a way to exhaust a process's
// memory from a laptop.
func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "that request body is not valid: "+err.Error())
		return false
	}
	return true
}

// badRequest reports a caller's mistake with its own message.
//
// Safe because every error that reaches here is one this package wrote for
// a person to read: a password too short, an address already taken, an
// expired invitation. A store error goes through fail instead.
func (s *Server) badRequest(w http.ResponseWriter, err error) {
	code := "bad_request"
	status := http.StatusBadRequest
	if errors.Is(err, control.ErrEmailTaken) {
		code, status = "email_taken", http.StatusConflict
	}
	writeError(w, status, code, err.Error())
}

// fail reports a server-side error without telling the caller what broke.
//
// The log gets the detail; the caller gets a sentence. A database error
// carries table and column names, and those are a map of the schema to
// somebody probing.
func (s *Server) fail(w http.ResponseWriter, err error) {
	s.log.Error("control plane request failed", slog.String("error", err.Error()))
	writeError(w, http.StatusInternalServerError, "internal",
		"something went wrong on the server. The log has the detail")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{
		"code": code, "message": message,
	}})
}

// events serves the access trail.
//
// Admin only, because it names who did what and when, which is an access
// review for an administrator and a map of who to target for anybody else.
//
// Separate from the engine's /v1/audit, which records decisions about queries.
// Two trails because they answer different questions and have nothing in
// common but a timestamp: one says "this query was refused for inflating a
// sum", the other says "this person was made an admin".
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	list, err := s.store.Events(r.Context(), u.OrgID, limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": list, "count": len(list)})
}
