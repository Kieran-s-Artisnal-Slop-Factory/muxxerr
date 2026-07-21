// Package web is the multiplexer's own interface: signing in, choosing an
// app, managing an account, and administering the server.
//
// Everything here is server-rendered Go templates and plain form posts. That
// is a deliberate choice rather than a shortcut. This is the layer that
// stands between a person and their data, and it has to keep working when a
// bundle fails to load, when a browser is old, and when scripting is off. It
// also means the gateway builds with the Go toolchain alone — no node, no
// lockfile, no build step for the thing whose job is to run other people's
// build steps.
package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"local-multiplexer/internal/auth"
	"local-multiplexer/internal/config"
	"local-multiplexer/internal/store"
	"local-multiplexer/internal/supervisor"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

const (
	sessionCookie = "mux_session"
	csrfCookie    = "mux_csrf"
	flashCookie   = "mux_flash"
	staticPrefix  = "/_mux/static"

	// SettingSignups is the runtime override for site.signups_enabled, so an
	// admin can close registrations without editing apps.json and restarting.
	SettingSignups = "signups_enabled"
)

// Throttle policy. Three free attempts, then a doubling lockout from one
// second to five minutes. The intent is to make online guessing pointless
// while never permanently locking out someone who simply forgot which
// password they used.
const (
	throttleFree = 3
	throttleBase = time.Second
	throttleMax  = 5 * time.Minute
)

// Exporter is the gateway capability the admin pages need. Keeping it an
// interface means web does not import gateway, and the two can be tested
// apart.
type Exporter interface {
	ExportDB(w http.ResponseWriter, r *http.Request, username string, app *config.App)
}

// Server holds everything the handlers need.
type Server struct {
	cfg         *config.Config
	store       *store.Store
	pepper      auth.Pepper
	sup         *supervisor.Supervisor
	exporter    Exporter
	tmpl        map[string]*template.Template
	siteName    string
	resetTokens *resetTokenStore
}

// New parses every page template up front: a template error should stop the
// process at boot, not surface as a 500 the first time somebody visits an
// unusual page.
func New(cfg *config.Config, st *store.Store, pepper auth.Pepper, sup *supervisor.Supervisor, exporter Exporter) (*Server, error) {
	s := &Server{
		cfg: cfg, store: st, pepper: pepper, sup: sup, exporter: exporter,
		tmpl:        map[string]*template.Template{},
		siteName:    "Multiplexer",
		resetTokens: newResetTokenStore(),
	}
	pages, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, p := range pages {
		name := strings.TrimSuffix(strings.TrimPrefix(p, "templates/"), ".html")
		if name == "layout" {
			continue
		}
		// Each page is its own set, parsed with the layout. Disjoint sets mean
		// one page's "content" block can never leak into another's.
		t, err := template.New(name).Funcs(funcs).ParseFS(templateFS, "templates/layout.html", p)
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", p, err)
		}
		s.tmpl[name] = t
	}
	for _, required := range []string{"login", "signup", "passphrase", "reset", "chooser", "account", "admin", "error"} {
		if _, ok := s.tmpl[required]; !ok {
			return nil, fmt.Errorf("missing template %s.html", required)
		}
	}
	return s, nil
}

// SetExporter wires the database-export capability after construction. The
// gateway needs the web server (as its Authenticator) and the web server needs
// the gateway (as its Exporter); breaking that cycle with a setter is simpler
// than an extra indirection layer for one method.
func (s *Server) SetExporter(e Exporter) { s.exporter = e }

var funcs = template.FuncMap{
	"since": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return t.Local().Format("2006-01-02 15:04")
	},
}

// PageData is the part of every view model the layout uses.
type PageData struct {
	Title     string
	SiteName  string
	User      *store.User
	Flash     string
	Error     string
	CSRFField template.HTML
	StaticURL string
}

func (s *Server) page(w http.ResponseWriter, r *http.Request, title string) PageData {
	return PageData{
		Title:     title,
		SiteName:  s.siteName,
		User:      s.UserFor(r),
		Flash:     takeFlash(w, r),
		CSRFField: s.csrfField(w, r),
		StaticURL: staticPrefix,
	}
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, code int, data any) {
	t, ok := s.tmpl[name]
	if !ok {
		slog.Error("unknown template", "name", name)
		http.Error(w, "template missing", http.StatusInternalServerError)
		return
	}
	// Render into memory first: a template that fails half way through would
	// otherwise emit a 200 with a truncated page and no way to correct it.
	var buf strings.Builder
	if err := t.ExecuteTemplate(&buf, "layout", data); err != nil {
		slog.Error("render template", "name", name, "error", err)
		http.Error(w, "Something went wrong rendering this page.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	fmt.Fprint(w, buf.String())
}

// ---------------------------------------------------------------- errors

type errorView struct {
	Page    PageData
	Code    int
	Message string
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, code int, msg string) {
	p := s.page(w, r, http.StatusText(code))
	s.render(w, r, "error", code, errorView{Page: p, Code: code, Message: msg})
}

// -------------------------------------------------------------- sessions

// UserFor implements gateway.Authenticator.
func (s *Server) UserFor(r *http.Request) *store.User {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	u, err := s.store.SessionUser(r.Context(), c.Value)
	if err != nil {
		return nil
	}
	return u
}

// LoginURL implements gateway.Authenticator.
func (s *Server) LoginURL(next string) string {
	if next == "" {
		return "/login"
	}
	return "/login?next=" + url.QueryEscape(next)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u *store.User) error {
	token, err := auth.NewSessionToken()
	if err != nil {
		return err
	}
	ttl := s.cfg.Site.SessionTTL.D()
	if err := s.store.CreateSession(r.Context(), token, u, ttl, r.UserAgent(), clientIP(r)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:  sessionCookie,
		Value: token,
		Path:  "/",
		// The cookie must reach /kieran/readerr/ as well as /login, so the
		// path is the whole site. HttpOnly keeps it away from app JavaScript,
		// which matters here more than usual: the apps are third-party-ish
		// code running on the same origin as this session.
		HttpOnly: true,
		Secure:   s.cfg.Site.SecureCookies,
		// Lax rather than Strict so that following a link into an app from
		// elsewhere still arrives authenticated; Strict would show a logged-out
		// page on every inbound link, and users would re-authenticate reflexively,
		// which is a worse habit to build than the residual CSRF risk that the
		// token below already covers.
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(ttl),
		MaxAge:   int(ttl.Seconds()),
	})
	return nil
}

func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		_ = s.store.DeleteSession(r.Context(), c.Value)
	}
	clearCookie(w, sessionCookie, "/", s.cfg.Site.SecureCookies)
}

func clearCookie(w http.ResponseWriter, name, path string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: path,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
		Expires: time.Unix(0, 0), MaxAge: -1,
	})
}

// clientIP prefers the socket address. X-Forwarded-For is honoured only when
// the immediate peer is loopback, because anywhere else it is just a header
// the client chose — and using it unconditionally would let anyone forge the
// key the login throttle counts against.
func clientIP(r *http.Request) string {
	host, _, err := splitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if isLoopbackAddr(host) {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			if first, _, ok := strings.Cut(fwd, ","); ok {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(fwd)
		}
	}
	return host
}

// ------------------------------------------------------------------ CSRF

// CSRF here is a double-submit token: a random value in a Lax cookie that
// must be echoed in a hidden form field. SameSite=Lax already stops a
// cross-site POST from carrying the session cookie at all, so this is the
// second layer rather than the first — but it is cheap, and it also catches
// the case of a browser that does not enforce SameSite.
func (s *Server) csrfToken(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) >= 32 {
		return c.Value
	}
	token, err := auth.NewSessionToken()
	if err != nil {
		return ""
	}
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookie, Value: token, Path: "/",
		HttpOnly: true, Secure: s.cfg.Site.SecureCookies,
		SameSite: http.SameSiteLaxMode, MaxAge: 12 * 3600,
	})
	// Make it visible to a same-request read, since the handler may render a
	// form before the browser has echoed the cookie back.
	r.AddCookie(&http.Cookie{Name: csrfCookie, Value: token})
	return token
}

func (s *Server) csrfField(w http.ResponseWriter, r *http.Request) template.HTML {
	tok := s.csrfToken(w, r)
	return template.HTML(`<input type="hidden" name="csrf_token" value="` +
		template.HTMLEscapeString(tok) + `">`)
}

func (s *Server) checkCSRF(w http.ResponseWriter, r *http.Request) bool {
	c, err := r.Cookie(csrfCookie)
	if err != nil || c.Value == "" {
		s.fail(w, r, http.StatusForbidden,
			"Your session form token is missing or expired. Go back, reload the page, and try again.")
		return false
	}
	if subtleCompare(c.Value, r.PostFormValue("csrf_token")) {
		return true
	}
	s.fail(w, r, http.StatusForbidden,
		"That form could not be verified. Go back, reload the page, and try again.")
	return false
}

// ----------------------------------------------------------------- flash

// setFlash stashes a one-shot message across a redirect. A cookie rather than
// server-side state so that nothing has to be cleaned up later.
func (s *Server) setFlash(w http.ResponseWriter, msg string) {
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: url.QueryEscape(msg), Path: "/",
		HttpOnly: true, Secure: s.cfg.Site.SecureCookies,
		SameSite: http.SameSiteLaxMode, MaxAge: 60,
	})
}

func takeFlash(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(flashCookie)
	if err != nil || c.Value == "" {
		return ""
	}
	clearCookie(w, flashCookie, "/", false)
	msg, err := url.QueryUnescape(c.Value)
	if err != nil {
		return ""
	}
	return msg
}

// ------------------------------------------------------------- redirects

// safeNext refuses anything that is not a local path. Without this check the
// post-login redirect is an open redirect, which is the classic way to make a
// phishing link look like it points at the real site.
func safeNext(next string) string {
	if next == "" {
		return ""
	}
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return ""
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return ""
	}
	return next
}

// nextForUser keeps a redirect target only if it belongs to the user who just
// signed in. Landing at /login from /kieran/readerr and then authenticating as
// alex must not send alex into kieran's namespace — they would only get a 403,
// but the right answer is their own dashboard.
func (s *Server) nextForUser(next string, u *store.User) string {
	next = safeNext(next)
	if next == "" {
		return "/"
	}
	trimmed := strings.Trim(next, "/")
	if trimmed == "" {
		return "/"
	}
	first := trimmed
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		first = trimmed[:i]
	}
	if config.Reserved(first) {
		return next
	}
	// The first segment is a username. Only honour it if it is theirs, or if
	// they are an admin allowed to look.
	if strings.EqualFold(first, u.Username) ||
		(u.IsAdmin && s.cfg.Site.AllowAdminImpersonation) {
		return next
	}
	return "/"
}

// --------------------------------------------------------------- helpers

func (s *Server) signupsOpen(ctx context.Context) bool {
	return s.store.BoolSetting(ctx, SettingSignups, s.cfg.Site.SignupsEnabled)
}

// audit records an action, logging rather than failing if the write does not
// land: an audit failure must never break the operation it describes.
func (s *Server) audit(r *http.Request, actor, action, target, detail string) {
	if err := s.store.Audit(r.Context(), actor, action, target, detail, clientIP(r)); err != nil {
		slog.Warn("audit write failed", "action", action, "error", err)
	}
}

func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) *store.User {
	u := s.UserFor(r)
	if u == nil {
		next := r.URL.Path
		if r.URL.RawQuery != "" {
			next += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, s.LoginURL(next), http.StatusSeeOther)
		return nil
	}
	return u
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) *store.User {
	u := s.requireUser(w, r)
	if u == nil {
		return nil
	}
	if !u.IsAdmin {
		s.fail(w, r, http.StatusForbidden, "That page is for administrators.")
		return nil
	}
	return u
}

// StaticHandler serves the gateway's own stylesheet and assets.
func (s *Server) StaticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		slog.Error("static fs", "error", err)
		return http.NotFoundHandler()
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.StripPrefix(staticPrefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Embedded assets change only when the binary does.
		w.Header().Set("Cache-Control", "public, max-age=3600")
		fileServer.ServeHTTP(w, r)
	}))
}

var errBadCredentials = errors.New("bad credentials")
