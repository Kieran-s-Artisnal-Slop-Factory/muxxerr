// Compatibility shim for apps that call their API at the origin root.
//
// The frontends resolve page links through a base-aware helper, so pages,
// assets, the manifest and the service worker all move cleanly under a
// /<user>/<app>/ prefix. Their sync layer does not: getSyncUrl() falls back to
// the empty string, so fetch(`${base}/sync/push`) becomes a request for
// /sync/push at the origin root, with no hint of which tenant it belongs to.
//
// Rather than require every app to be patched before it can be hosted here
// (patches/01-sync-base.md is the clean fix, and is worth applying), the
// gateway recovers the missing context from the Referer. A same-origin fetch
// from /kieran/readerr/settings/ carries that full path under every browser's
// default referrer policy, which is exactly the prefix the request should have
// had.
//
// This is a fallback, and it is documented as one. It fails if a page ever
// sets Referrer-Policy: no-referrer, and it cannot help a request made from a
// context with no referrer at all. When it cannot tell which instance is
// meant, it says so plainly instead of guessing — routing a user's data into
// somebody else's database would be far worse than an error message.
package gateway

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"local-multiplexer/internal/config"
)

// ShimHandler routes a root-absolute API request to the instance implied by
// its Referer. Mount it on the union of every app's API prefixes.
func (g *Gateway) ShimHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, appName, ok := g.inferInstance(r)
		if !ok {
			writeGatewayError(w, r, http.StatusNotFound,
				"This is a sync endpoint for an app, but the request did not say which one. "+
					"Open the app from your dashboard rather than calling this URL directly.")
			return
		}
		slog.Debug("shim routed root API call", "user", username, "app", appName, "path", r.URL.Path)

		// Re-enter the normal path with the prefix restored, so authorisation,
		// guards, provisioning checks and proxying all behave identically to a
		// properly-prefixed request. Rewriting the URL rather than duplicating
		// the logic is the point.
		rr := r.Clone(r.Context())
		rr.URL.Path = "/" + username + "/" + appName + r.URL.Path
		rr.SetPathValue("user", username)
		rr.SetPathValue("app", appName)
		rr.Header.Set("X-Mux-Shimmed", "referer")
		g.serve(w, rr)
	})
}

// inferInstance recovers (user, app) for a root-absolute API call.
func (g *Gateway) inferInstance(r *http.Request) (string, string, bool) {
	// An explicit header wins. Nothing sends it today, but it gives a patched
	// app or a script a way to be unambiguous without relying on Referer.
	if v := r.Header.Get("X-Mux-Instance"); v != "" {
		if user, app, ok := strings.Cut(strings.Trim(v, "/"), "/"); ok {
			if _, known := g.cfg.App(app); known {
				return strings.ToLower(user), app, true
			}
		}
	}

	ref := r.Header.Get("Referer")
	if ref == "" {
		return "", "", false
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "", "", false
	}
	// Only trust a same-origin referrer. A cross-site page must not be able to
	// nominate which tenant a request lands in — the session cookie is
	// SameSite=Lax, but this is the belt to that braces.
	if u.Host != "" && r.Host != "" && !strings.EqualFold(u.Host, r.Host) {
		return "", "", false
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", false
	}
	username, appName := strings.ToLower(parts[0]), parts[1]
	if config.Reserved(username) {
		return "", "", false
	}
	if _, ok := g.cfg.App(appName); !ok {
		return "", "", false
	}
	return username, appName, true
}

// APIPrefixPatterns returns the ServeMux patterns the shim must be mounted on:
// the union of every app's API prefixes, minus the paths the gateway serves
// itself. Registering them explicitly is what stops "/sync/push" from being
// mistaken for the app "push" belonging to the user "sync".
func APIPrefixPatterns(cfg *config.Config) []string {
	seen := map[string]bool{}
	// /healthz belongs to the gateway. The apps' own connection test hits it
	// and gets the same {"status":"ok"} shape, so nothing needs the app's.
	skip := map[string]bool{"/healthz": true, "/": true}

	var out []string
	for i := range cfg.Apps {
		for _, p := range cfg.Apps[i].APIPrefixes {
			if skip[p] || seen[p] || !strings.HasPrefix(p, "/") {
				continue
			}
			// A prefix like "/sync/" is a subtree pattern in net/http; an exact
			// path like "/title" is matched exactly. Both are more specific
			// than "/{user}/{app}/", so the shim wins the routing contest.
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}
