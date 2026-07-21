// The handful of app files that must be readable without a session.
//
// A browser fetches <link rel="manifest"> with credentials OMITTED — it is a
// same-origin request that deliberately carries no cookies unless the tag says
// crossorigin="use-credentials". So the gateway saw an anonymous request for
// /kieran/workoutt/manifest.webmanifest, correctly decided it was not a
// navigation, and answered 401. The console filled with "Manifest fetch
// failed, code 401" and the apps stopped being installable as PWAs.
//
// Two fixes were possible. Adding crossorigin="use-credentials" to every app's
// Layout.astro works, but it is an upstream change to every app including ones
// that do not exist yet, for a file that is not secret. The manifest and the
// icons are build artifacts: byte-identical for every user of an app, sitting
// in the runtime directory, containing nothing but the app's name and colours.
// So the gateway serves them straight off disk, before the auth check, without
// starting a child process.
//
// What this deliberately does NOT do is confirm that the user exists. The
// bytes do not depend on the username, so answering identically for
// /kieran/readerr/icon.svg and /nosuchperson/readerr/icon.svg avoids handing
// out a user-enumeration oracle in exchange for nothing.
package gateway

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"muxerr/internal/config"
)

// publicAssets are served without a session. Every entry has to be justified
// as "identical for all users and not worth hiding" — that is the whole test.
// The manifest is the one that must be here; the icons are here because the
// manifest references them and a PWA install prompt fetches them the same
// credential-less way.
var publicAssets = map[string]bool{
	"manifest.webmanifest": true,
	"manifest.json":        true,
	"favicon.ico":          true,
	"favicon.svg":          true,
	"icon.svg":             true,
	"icon-maskable.svg":    true,
	"apple-touch-icon.png": true,
	"icon-192.png":         true,
	"icon-512.png":         true,
	"robots.txt":           true,
}

// IsPublicAsset reports whether an instance-relative path may be served
// anonymously. Only a bare filename at the root of the app counts: nesting
// would let a crafted path reach further into the dist directory than this
// list implies.
func IsPublicAsset(p string) bool {
	name := strings.TrimPrefix(p, "/")
	if name == "" || strings.Contains(name, "/") {
		return false
	}
	return publicAssets[name]
}

// ServePublicAsset writes an app's public file with the usual base rewriting,
// so a relative manifest still resolves under the tenant's prefix. It reports
// whether it handled the request; false means the caller should carry on with
// the normal authenticated path.
func (g *Gateway) ServePublicAsset(w http.ResponseWriter, r *http.Request, username, appName, assetPath string) bool {
	app, ok := g.cfg.App(appName)
	if !ok || !IsPublicAsset(assetPath) {
		return false
	}
	name := strings.TrimPrefix(assetPath, "/")
	// path.Clean plus the no-slash rule in IsPublicAsset means this can only
	// ever be a direct child of the dist directory, but join through
	// filepath.Base as well so nothing about future edits to that list can
	// turn this into a traversal.
	full := filepath.Join(g.cfg.DistDir(app), filepath.Base(path.Clean(name)))
	f, err := os.Open(full)
	if err != nil {
		return false // not built, or this app has no such file: fall through
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		return false
	}

	rc := &reqCtx{
		username:     strings.ToLower(username),
		app:          app,
		prefix:       "/" + strings.ToLower(username) + "/" + app.Name,
		upstreamPath: "/" + name,
		acceptsGzip:  strings.Contains(r.Header.Get("Accept-Encoding"), "gzip"),
	}

	// A manifest referencing "./icon.svg" resolves against its own URL, so it
	// needs no rewriting — but an app built with the sentinel base may well
	// have absolute paths in it, and the same rewriter handles both.
	rr := &rewriteRecorder{ResponseWriter: w, rc: rc, req: r}
	// Public, unauthenticated, and immutable until the next build. Let the
	// browser keep it, but revalidate daily so a rebuild is picked up.
	rr.Header().Set("Cache-Control", "public, max-age=86400")
	// Go's mime table has no entry for .webmanifest, so ServeContent would
	// sniff it as text/plain. Chrome accepts that but complains, and the spec
	// is unambiguous, so say it properly.
	if ct := publicContentType(name); ct != "" {
		rr.Header().Set("Content-Type", ct)
	}
	http.ServeContent(rr, r, fi.Name(), fi.ModTime(), f)
	rr.finish()
	return true
}

// publicContentType names the types Go's mime table gets wrong or does not
// know. Anything not listed keeps whatever ServeContent works out.
func publicContentType(name string) string {
	switch path.Ext(name) {
	case ".webmanifest":
		return "application/manifest+json"
	case ".ico":
		// Go says image/x-icon; the registered type is image/vnd.microsoft.icon
		// and every browser accepts either, so this is tidiness rather than a
		// fix. Left explicit so the whole set is described in one place.
		return "image/x-icon"
	default:
		return ""
	}
}

// PublicAssetNames is used by the dashboard to find an app's icon on disk.
func PublicAssetNames() []string {
	out := make([]string, 0, len(publicAssets))
	for name := range publicAssets {
		out = append(out, name)
	}
	return out
}

// FindIcon returns the best available icon filename for an app, or "" if the
// build has none. Order is deliberate: a scalable icon beats a raster one, and
// a purpose-built app icon beats the browser-tab favicon.
func FindIcon(cfg *config.Config, app *config.App) string {
	for _, name := range []string{"icon.svg", "favicon.svg", "icon-192.png", "apple-touch-icon.png", "favicon.ico"} {
		if _, err := os.Stat(filepath.Join(cfg.DistDir(app), name)); err == nil {
			return name
		}
	}
	return ""
}
