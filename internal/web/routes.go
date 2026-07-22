package web

import (
	"log/slog"
	"net/http"
	"strings"

	"muxxerr/internal/config"
	"muxxerr/internal/gateway"
)

// Routes builds the whole URL space.
//
// Every app lives at /<username>/<app>/, which is tempting to express as the
// pattern "/{user}/{app}/". It cannot be: net/http rejects that alongside the
// shim's "/sync/" as an unresolvable conflict, because "/sync//" matches both
// and neither pattern is more specific than the other. ServeMux is right to
// refuse — a two-segment wildcard really is ambiguous against a one-segment
// subtree.
//
// So the wildcard is not registered at all. Everything the gateway owns gets
// an explicit pattern, and the catch-all at "/" does the app dispatch by hand:
// if the second path segment names a configured app and the first is not a
// reserved word, it is an app request. That is a few more lines than a pattern
// would have been, and in exchange the routing table has no ambiguity in it
// for anyone to reason about later.
func Routes(s *Server, gw *gateway.Gateway, cfg *config.Config) http.Handler {
	mux := http.NewServeMux()

	// --- gateway's own endpoints
	mux.HandleFunc("GET /healthz", s.HandleHealthz)
	mux.HandleFunc("GET /robots.txt", s.HandleRobots)
	mux.Handle("GET "+staticPrefix+"/", s.StaticHandler())

	// --- identity
	mux.HandleFunc("GET /login", s.HandleLogin)
	mux.HandleFunc("POST /login", s.HandleLogin)
	mux.HandleFunc("POST /logout", s.HandleLogout)
	mux.HandleFunc("GET /signup", s.HandleSignup)
	mux.HandleFunc("POST /signup", s.HandleSignup)
	mux.HandleFunc("GET /reset", s.HandleReset)
	mux.HandleFunc("POST /reset", s.HandleReset)
	mux.HandleFunc("POST /passphrase", s.HandlePassphraseAck)

	// --- the signed-in surface. One URL per action, matching the admin
	// routes below: the audit trail then records what was actually requested
	// rather than the value of a form field that could have been anything.
	mux.HandleFunc("GET /{$}", s.HandleRoot)
	mux.HandleFunc("GET /account", s.HandleAccount)
	mux.HandleFunc("POST /account/password", s.HandleChangePassword)
	mux.HandleFunc("POST /account/passphrase", s.HandleRotatePassphrase)
	mux.HandleFunc("POST /account/sessions/revoke", s.HandleRevokeSessions)
	mux.HandleFunc("POST /apps/{app}/install", s.HandleAddApp)
	mux.HandleFunc("POST /apps/{app}/remove", s.HandleRemoveApp)
	mux.HandleFunc("GET /apps/{app}/export", s.HandleExportOwn)
	mux.HandleFunc("GET /apps/{app}/logs", s.HandleLogs)

	// --- database tools. The viewer is always available and is read-only by
	// construction (it works on a snapshot in the browser); the console writes
	// to a live database and is off unless apps.json enables it.
	mux.HandleFunc("GET /tools/sqlite", s.HandleSQLiteViewer)
	mux.HandleFunc("GET /tools/sql", s.HandleSQLConsole)
	mux.HandleFunc("POST /tools/sql/unlock", s.HandleSQLConsoleUnlock)
	mux.HandleFunc("POST /tools/sql/execute", s.HandleSQLConsoleExecute)

	// --- administration
	mux.HandleFunc("GET /admin", s.HandleAdmin)
	mux.HandleFunc("POST /admin/users/{id}/disable", s.HandleAdminSetDisabled(true))
	mux.HandleFunc("POST /admin/users/{id}/enable", s.HandleAdminSetDisabled(false))
	mux.HandleFunc("POST /admin/users/{id}/admin", s.HandleAdminToggleAdmin)
	mux.HandleFunc("POST /admin/users/{id}/reset", s.HandleAdminReset)
	mux.HandleFunc("POST /admin/users/{id}/delete", s.HandleAdminDeleteUser)
	mux.HandleFunc("POST /admin/instances/{user}/{app}/stop", s.HandleAdminStopInstance)
	mux.HandleFunc("GET /admin/instances/{user}/{app}/export", s.HandleAdminExport)
	mux.HandleFunc("GET /admin/instances/{user}/{app}/logs", s.HandleLogs)
	mux.HandleFunc("POST /admin/settings/signups", s.HandleAdminSignups)

	// --- the compatibility shim for apps that call their API at the origin
	// root. See internal/gateway/shim.go for why this exists.
	shim := gw.ShimHandler()
	for _, pattern := range gateway.APIPrefixPatterns(cfg) {
		mux.Handle(pattern, shim)
		slog.Debug("mounted root API shim", "pattern", pattern)
	}

	// --- the apps themselves, plus everything unmatched
	appHandler := gw.Handler()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			s.HandleRoot(w, r)
			return
		}
		user, app, hasSlash, ok := splitAppPath(cfg, r.URL.Path)
		if !ok {
			s.fail(w, r, http.StatusNotFound, "There is nothing at that address.")
			return
		}
		// Canonicalise the username's case before anything else looks at the
		// path. The gateway authorises against the lowercased name but strips
		// the prefix by string match, so /Kieran/readerr/ would authorise as
		// kieran and then fail to strip — handing the child an unstripped path
		// and, worse, making the guarded-route check compare against the wrong
		// path and skip the SSRF policy entirely. One redirect removes the
		// whole class of mismatch.
		canonical := "/" + user + "/" + app
		if !strings.HasPrefix(r.URL.Path, canonical+"/") && r.URL.Path != canonical {
			target := canonical + strings.TrimPrefix(r.URL.Path, "/"+pathSegments(r.URL.Path, 2))
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusMovedPermanently)
			return
		}
		// The gateway reads these the same way it would from a pattern match,
		// so nothing downstream has to know the routing was done by hand.
		rr := r.Clone(r.Context())
		rr.SetPathValue("user", user)
		rr.SetPathValue("app", app)
		if !hasSlash {
			// Build the target from the validated segments rather than from
			// the raw path, so a crafted path can never become the redirect.
			http.Redirect(w, r, withQuery(canonical+"/", r.URL.RawQuery), http.StatusMovedPermanently)
			return
		}
		appHandler.ServeHTTP(w, rr)
	})

	return securityHeaders(mux)
}

// securityHeaders applies the headers that are safe to set globally.
//
// Note what is deliberately absent: a Content-Security-Policy. The gateway's
// own pages could carry a strict one, but the same middleware wraps the
// proxied apps, and those are third-party builds with inline scripts and their
// own expectations. Setting a policy here that suits this package would break
// them, and setting one loose enough for them would be theatre. If a CSP is
// wanted for the gateway's pages specifically, it belongs in render(), not
// here.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		// The apps must not be framed by another origin, and neither must the
		// login page — clickjacking a "delete my account" button is a real
		// thing, and nothing here needs to be embeddable.
		h.Set("X-Frame-Options", "SAMEORIGIN")
		// Same-origin referrers only: the shim depends on the path being
		// present for same-origin requests, and no outside site needs to learn
		// which apps somebody uses from a referrer.
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// splitAppPath decides whether a path addresses an app instance, and returns
// the pieces if it does.
//
// hasSlash distinguishes /kieran/readerr from /kieran/readerr/ — the bare form
// has to redirect, because Astro emits directory-style pages and every
// relative URL on the landing page would otherwise resolve one level too high.
func splitAppPath(cfg *config.Config, path string) (user, app string, hasSlash, ok bool) {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return "", "", false, false
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false, false
	}
	user, app = strings.ToLower(parts[0]), parts[1]
	// The first segment must look like a username, not merely be non-empty.
	// A request for /%2Fevil.example/readerr decodes to a path whose first
	// segment is "" and second is "evil.example" — or, with other encodings,
	// to something that is not a name at all. Letting those through meant the
	// redirect below could be handed a protocol-relative target and turn into
	// an open redirect, and it meant the prefix the gateway strips could
	// differ from the one it authorised.
	if !usernameRe.MatchString(user) || config.Reserved(user) {
		return "", "", false, false
	}
	if _, known := cfg.App(app); !known {
		return "", "", false, false
	}
	// Deeper paths are always inside the app; only the bare two-segment form
	// can be missing its trailing slash.
	hasSlash = len(parts) > 2 || strings.HasSuffix(path, "/")
	return user, app, hasSlash, true
}

// pathSegments returns the first n segments of a path, joined, with no
// surrounding slashes: pathSegments("/Kieran/readerr/settings/", 2) is
// "Kieran/readerr".
func pathSegments(path string, n int) string {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < n {
		n = len(parts)
	}
	return strings.Join(parts[:n], "/")
}

func withQuery(path, rawQuery string) string {
	if rawQuery == "" {
		return path
	}
	return path + "?" + rawQuery
}

// PathIsApp reports whether a URL path looks like it addresses an app
// instance. Used by the request logger to keep the noisy asset traffic at a
// lower level than the interesting requests.
func PathIsApp(cfg *config.Config, path string) bool {
	_, _, _, ok := splitAppPath(cfg, path)
	return ok
}
