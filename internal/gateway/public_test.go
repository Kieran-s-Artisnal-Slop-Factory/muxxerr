package gateway

import "testing"

// The manifest and icons are the only things served without a session, so the
// rule for what counts must be exact. A path that escapes the app's dist
// directory, or that reaches a file this list did not intend, would be an
// unauthenticated read of whatever it landed on.
func TestIsPublicAsset(t *testing.T) {
	yes := []string{
		"/manifest.webmanifest",
		"manifest.webmanifest",
		"/favicon.ico",
		"/icon.svg",
		"/icon-maskable.svg",
		"/robots.txt",
	}
	for _, p := range yes {
		if !IsPublicAsset(p) {
			t.Errorf("IsPublicAsset(%q) = false, want true", p)
		}
	}

	no := []string{
		"",
		"/",
		"/index.html",               // the app shell is behind auth
		"/sync/pull",                // API
		"/settings/",                // a page
		"/_astro/app.js",            // build assets stay authenticated
		"/sub/manifest.webmanifest", // nesting is not allowed
		"/../manifest.webmanifest",  // traversal
		"/../../data/mux.db",        //
		"/icon.svg/../../../secret", //
		"manifest.webmanifest/",     // trailing slash makes it a directory
		"/Manifest.webmanifest",     // the list is case-sensitive on purpose
		"/manifest.webmanifest?x=1", // query is not part of the path here
	}
	for _, p := range no {
		if IsPublicAsset(p) {
			t.Errorf("IsPublicAsset(%q) = true, want false", p)
		}
	}
}

func TestPublicContentType(t *testing.T) {
	cases := map[string]string{
		"manifest.webmanifest": "application/manifest+json",
		"favicon.ico":          "image/x-icon",
		"icon.svg":             "", // let ServeContent decide
		"robots.txt":           "",
	}
	for name, want := range cases {
		if got := publicContentType(name); got != want {
			t.Errorf("publicContentType(%q) = %q, want %q", name, got, want)
		}
	}
}
