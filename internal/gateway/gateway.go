// Package gateway mounts single-tenant apps at /<username>/<app>/.
//
// The apps it fronts were written to own a whole origin: their routes are
// root-anchored, their frontends are built for "/", and they have no concept
// of a user. Rather than teach every app about tenancy, the gateway keeps
// that illusion intact and does the translation at the edge:
//
//   - Each (user, app) pair gets its own child process and its own SQLite
//     file, so isolation is a property of the operating system rather than of
//     a WHERE clause somebody has to remember to write.
//   - Requests are proxied with the /<user>/<app> prefix stripped, so the
//     child still believes it is mounted at the root.
//   - Responses are rewritten on the way out, replacing the sentinel base the
//     frontend was built with (/__MUX__) with the real per-user prefix. That
//     is what lets ONE build of an app serve every user — see rewrite.go.
//
// The alternative designs were considered and rejected: importing the apps as
// libraries fails because they are separate Go modules, all package main,
// configured through process-global os.Getenv; building the frontend once per
// user costs a full Astro build and ~5 MB of disk per (user, app).
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"muxerr/internal/config"
	"muxerr/internal/store"
	"muxerr/internal/supervisor"
)

// maxAPIBody caps a proxied request body. The apps decode /sync/push with an
// unbounded json.Decoder, so without this one client can make the server
// allocate arbitrarily. The limit is deliberately generous — a first push of
// a large library is legitimate — it exists to stop abuse, not to shape use.
const maxAPIBody = 256 << 20 // 256 MiB

// Authenticator resolves the caller's identity. The web package implements
// it; the gateway only needs the question answered, not the mechanism.
type Authenticator interface {
	// UserFor returns the logged-in user, or nil if the request carries no
	// valid session.
	UserFor(r *http.Request) *store.User
	// LoginURL is where an unauthenticated navigation should be sent, with
	// next preserved so the user lands back where they were aiming.
	LoginURL(next string) string
}

// Gateway routes and proxies app traffic.
type Gateway struct {
	cfg    *config.Config
	store  *store.Store
	sup    *supervisor.Supervisor
	auth   Authenticator
	proxy  *httputil.ReverseProxy
	static map[string]http.Handler // app name -> file server for static apps
}

// New wires a gateway. The ReverseProxy is shared across every instance: the
// per-request target is chosen in the Director from the request context, so
// there is no need for one proxy object per child.
func New(cfg *config.Config, st *store.Store, sup *supervisor.Supervisor, auth Authenticator) *Gateway {
	g := &Gateway{cfg: cfg, store: st, sup: sup, auth: auth, static: map[string]http.Handler{}}

	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		// Deliberately no ResponseHeaderTimeout: a first /sync/pull with
		// since=0 materialises the whole database into one response, and the
		// apps build it fully in memory before writing a byte. A timeout here
		// would turn a slow first sync into a hard failure.
		DisableCompression: true,
	}

	g.proxy = &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			rc := requestCtx(pr.In.Context())
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = rc.target.Host
			pr.Out.URL.Path = rc.upstreamPath
			pr.Out.URL.RawPath = ""
			pr.Out.Host = rc.target.Host

			// Tell the app who is asking. It ignores these today, but a
			// generated app can use them, and they make the child's own logs
			// attributable.
			pr.Out.Header.Set("X-Mux-User", rc.username)
			pr.Out.Header.Set("X-Mux-App", rc.app.Name)
			pr.Out.Header.Set("X-Mux-Prefix", rc.prefix)
			pr.SetXForwarded()

			if !rc.isAPI {
				// Frontend responses get string-rewritten, so they must not
				// arrive compressed, and a byte Range would be measured
				// against the pre-rewrite body. We re-compress on the way out
				// (see rewrite.go) so nothing is lost on the wire.
				pr.Out.Header.Del("Accept-Encoding")
				pr.Out.Header.Del("Range")
				pr.Out.Header.Del("If-Range")
			}
		},
		ModifyResponse: g.modifyResponse,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			rc := requestCtx(r.Context())
			slog.Error("proxy failed", "user", rc.username, "app", rc.appName(), "path", r.URL.Path, "error", err)
			if errors.Is(err, context.Canceled) {
				return // the client went away; nothing to report to
			}
			writeGatewayError(w, r, http.StatusBadGateway,
				"The app is not responding. It may still be starting up — try again in a moment.")
		},
		FlushInterval: 200 * time.Millisecond,
	}

	for i := range cfg.Apps {
		a := &cfg.Apps[i]
		if a.Kind == config.KindStatic {
			g.static[a.Name] = http.FileServer(http.Dir(cfg.DistDir(a)))
		}
	}
	return g
}

// reqCtx carries everything resolved about a request from routing through to
// the proxy's Rewrite and ModifyResponse hooks.
type reqCtx struct {
	username     string
	app          *config.App
	prefix       string // "/kieran/readerr"
	upstreamPath string // the path with the prefix stripped, always starts with /
	isAPI        bool
	target       *url.URL
	// acceptsGzip records what the ORIGINAL client asked for. We strip
	// Accept-Encoding before the request reaches the child so that rewritable
	// responses arrive as plain text, which makes this the only surviving
	// record of whether to re-compress on the way back out.
	acceptsGzip bool
}

func (rc *reqCtx) appName() string {
	if rc == nil || rc.app == nil {
		return ""
	}
	return rc.app.Name
}

type ctxKey struct{}

func requestCtx(ctx context.Context) *reqCtx {
	rc, _ := ctx.Value(ctxKey{}).(*reqCtx)
	if rc == nil {
		return &reqCtx{}
	}
	return rc
}

// Handler serves /{user}/{app}/... — mount it on those patterns.
func (g *Gateway) Handler() http.Handler {
	return http.HandlerFunc(g.serve)
}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	username := strings.ToLower(r.PathValue("user"))
	appName := r.PathValue("app")

	app, ok := g.cfg.App(appName)
	if !ok {
		writeGatewayError(w, r, http.StatusNotFound, "No such app: "+appName)
		return
	}
	prefix := "/" + username + "/" + app.Name

	// --- the PWA manifest and its icons, before any auth check. The browser
	// fetches these without cookies, so requiring a session makes every app
	// permanently un-installable. See public.go.
	if assetPath := strings.TrimPrefix(r.URL.Path, prefix); IsPublicAsset(assetPath) {
		if g.ServePublicAsset(w, r, username, app.Name, assetPath) {
			return
		}
	}

	// --- identity
	caller := g.auth.UserFor(r)
	if caller == nil {
		g.challenge(w, r)
		return
	}

	// --- authorisation. An admin may look at another user's app only if the
	// operator has explicitly turned that on: exporting a database is already
	// possible from the admin page, and quietly browsing as somebody else is
	// a larger power than administration needs by default.
	owner := caller.Username == username
	if !owner && !(caller.IsAdmin && g.cfg.Site.AllowAdminImpersonation) {
		writeGatewayError(w, r, http.StatusForbidden, "This app belongs to another user.")
		return
	}

	ctx := r.Context()
	target, err := g.store.UserByName(ctx, username)
	if err != nil {
		writeGatewayError(w, r, http.StatusNotFound, "No such user: "+username)
		return
	}
	provisioned, err := g.store.HasInstance(ctx, target.ID, app.Name)
	if err != nil {
		slog.Error("instance lookup", "error", err)
		writeGatewayError(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	if !provisioned {
		if owner {
			// The user is aiming at an app they have not added yet. Send them
			// to the chooser rather than a dead end.
			http.Redirect(w, r, "/?add="+url.QueryEscape(app.Name), http.StatusSeeOther)
			return
		}
		writeGatewayError(w, r, http.StatusNotFound, "That user has not set up this app.")
		return
	}

	upstreamPath := strings.TrimPrefix(r.URL.Path, prefix)
	if upstreamPath == "" {
		upstreamPath = "/"
	}

	rc := &reqCtx{
		username:     username,
		app:          app,
		prefix:       prefix,
		upstreamPath: upstreamPath,
		isAPI:        isAPIPath(app, upstreamPath),
		acceptsGzip:  strings.Contains(r.Header.Get("Accept-Encoding"), "gzip"),
	}

	// --- SSRF policy on endpoints that fetch a caller-supplied URL.
	if msg, blocked := checkGuards(app, upstreamPath, r.URL.Query()); blocked {
		slog.Warn("blocked guarded request", "user", username, "app", app.Name, "path", upstreamPath, "reason", msg)
		writeGatewayError(w, r, http.StatusForbidden, msg)
		return
	}

	_ = g.store.TouchInstance(ctx, target.ID, app.Name)

	// --- static-only apps never get a child process.
	if app.Kind == config.KindStatic {
		g.serveStatic(w, r, rc)
		return
	}

	// --- ensure the child is up. Ensure blocks through cold start, which is
	// ~100ms for these apps; the timeout is for a migration-heavy boot.
	startCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	inst, err := g.sup.Ensure(startCtx, username, app)
	if err != nil {
		slog.Error("start instance", "user", username, "app", app.Name, "error", err)
		writeGatewayError(w, r, http.StatusServiceUnavailable,
			"Could not start "+app.Title+". Check the gateway logs.")
		return
	}
	inst.Touch()
	rc.target = inst.URL()

	if rc.isAPI {
		r.Body = http.MaxBytesReader(w, r.Body, maxAPIBody)
	}
	g.proxy.ServeHTTP(w, r.WithContext(context.WithValue(ctx, ctxKey{}, rc)))
}

// challenge asks an unauthenticated caller to log in, in the form that caller
// can actually act on.
//
// This distinction matters more than it looks. A 302 to the login page in
// response to the app's background fetch("/sync/push") is followed silently
// by fetch, and the app then tries to parse an HTML login page as its sync
// response — surfacing to the user as an inscrutable parse error rather than
// "you are logged out". API callers get a 401 and a JSON body instead.
func (g *Gateway) challenge(w http.ResponseWriter, r *http.Request) {
	if isNavigation(r) {
		next := r.URL.Path
		if r.URL.RawQuery != "" {
			next += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, g.auth.LoginURL(next), http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	fmt.Fprint(w, `{"error":"unauthenticated","detail":"Your session has expired. Reload the page to sign in again."}`)
}

// isNavigation reports whether this request is the browser loading a page, as
// opposed to script-initiated. Sec-Fetch-Mode is authoritative in every
// current browser; the Accept sniff is the fallback for anything older.
func isNavigation(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Mode") {
	case "navigate":
		return true
	case "cors", "no-cors", "same-origin", "websocket":
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// isAPIPath reports whether an instance-relative path is app API rather than
// frontend. API responses are passed through untouched — no body rewriting,
// no re-compression — because they are data, and a user's own note containing
// the sentinel string must never be silently edited on its way to them.
func isAPIPath(app *config.App, path string) bool {
	for _, p := range app.APIPrefixes {
		if strings.HasSuffix(p, "/") {
			if strings.HasPrefix(path, p) {
				return true
			}
			continue
		}
		if path == p || strings.HasPrefix(path, p+"?") {
			return true
		}
	}
	return false
}

// writeGatewayError renders an error in the shape the caller can use: JSON for
// script, a minimal HTML page for a person.
func writeGatewayError(w http.ResponseWriter, r *http.Request, code int, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	if !isNavigation(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"error":%q}`, msg)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%d</title>
<style>body{font:16px/1.6 system-ui,sans-serif;max-width:34rem;margin:15vh auto;padding:0 1.5rem;color:#222;background:#faf9f7}
h1{font-size:1.3rem;margin:0 0 .5rem}a{color:#2e6f4e}@media(prefers-color-scheme:dark){body{background:#16181a;color:#e6e3df}a{color:#7fc7a0}}</style>
<h1>%d — %s</h1><p>%s</p><p><a href="/">Back to your apps</a></p>`,
		code, code, http.StatusText(code), htmlEscape(msg))
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
