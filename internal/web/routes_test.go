package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"muxerr/internal/config"
)

func routeTestConfig(t *testing.T) *config.Config {
	t.Helper()
	c := &config.Config{
		Apps: []config.App{
			{Name: "readerr", Kind: config.KindSync},
			{Name: "workoutt", Kind: config.KindSync},
		},
	}
	return c
}

// TestSplitAppPathRejectsNonUsernames pins the fix for an unauthenticated open
// redirect. /%2Fevil.example/readerr decodes to a path whose first segment is
// empty and whose second looks like a host; the trailing-slash redirect used to
// be built from that raw path, so it emitted a protocol-relative Location and
// sent the visitor to another site. The first segment must look like a username
// or the path is simply not an app path.
func TestSplitAppPathRejectsNonUsernames(t *testing.T) {
	cfg := routeTestConfig(t)
	bad := []string{
		"//evil.example/readerr", // what /%2Fevil.example/readerr decodes to
		"/evil.example/readerr",  // dots are not allowed in usernames
		"/us er/readerr",         // spaces
		"/admin/readerr",         // reserved
		"/login/readerr",         // reserved
		"/apps/readerr",          // reserved: the install route's namespace
		"/account/password",      // reserved
		"/-leading/readerr",      // must start alphanumeric
		"/a/readerr",             // too short
		"/kieran/nosuchapp",      // unknown app
		"/kieran",                // only one segment
		"/",                      //
		"",                       //
		"/x/z",                   // unknown app
	}
	for _, p := range bad {
		if _, _, _, ok := splitAppPath(cfg, p); ok {
			t.Errorf("splitAppPath(%q) accepted it; it must not", p)
		}
	}

	good := []struct {
		path      string
		user, app string
		hasSlash  bool
	}{
		{"/kieran/readerr/", "kieran", "readerr", true},
		{"/kieran/readerr", "kieran", "readerr", false},
		{"/kieran/readerr/settings/", "kieran", "readerr", true},
		{"/kieran/workoutt/_astro/x.js", "kieran", "workoutt", true},
		{"/alex-2/readerr/", "alex-2", "readerr", true},
		{"/a_b/readerr/", "a_b", "readerr", true},
		// Mixed case is accepted here and canonicalised by the router.
		{"/KIERAN/readerr/", "kieran", "readerr", true},
	}
	for _, c := range good {
		user, app, hasSlash, ok := splitAppPath(cfg, c.path)
		if !ok {
			t.Errorf("splitAppPath(%q) rejected a valid app path", c.path)
			continue
		}
		if user != c.user || app != c.app || hasSlash != c.hasSlash {
			t.Errorf("splitAppPath(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.path, user, app, hasSlash, c.user, c.app, c.hasSlash)
		}
	}
}

// TestPathSegments covers the helper the case-canonicalising redirect uses to
// find where the /<user>/<app> prefix ends in the original path.
func TestPathSegments(t *testing.T) {
	cases := []struct{ path, want string }{
		{"/Kieran/readerr/settings/", "Kieran/readerr"},
		{"/Kieran/readerr", "Kieran/readerr"},
		{"/Kieran", "Kieran"},
		{"/", ""},
	}
	for _, c := range cases {
		if got := pathSegments(c.path, 2); got != c.want {
			t.Errorf("pathSegments(%q,2) = %q, want %q", c.path, got, c.want)
		}
	}
	// The remainder-splicing the redirect does must reconstruct the tail
	// exactly, whatever the case of the prefix.
	path := "/KIERAN/readerr/settings/deep?x=1"
	rest := strings.TrimPrefix(strings.SplitN(path, "?", 2)[0], "/"+pathSegments(path, 2))
	if rest != "/settings/deep" {
		t.Errorf("remainder = %q, want %q", rest, "/settings/deep")
	}
}

func TestWithQuery(t *testing.T) {
	if got := withQuery("/kieran/readerr/", ""); got != "/kieran/readerr/" {
		t.Errorf("withQuery with no query = %q", got)
	}
	if got := withQuery("/kieran/readerr/", "a=1&b=2"); got != "/kieran/readerr/?a=1&b=2" {
		t.Errorf("withQuery = %q", got)
	}
}

// TestServeMuxCleansTraversal pins an assumption the routing relies on rather
// than re-implements: net/http normalises "." and ".." out of a request path
// and redirects, so splitAppPath never has to defend against traversal in the
// segments after the app name. If a future Go release changed that, this fails
// and the assumption gets revisited instead of silently becoming wrong.
func TestServeMuxCleansTraversal(t *testing.T) {
	mux := http.NewServeMux()
	var saw string
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { saw = r.URL.Path })

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, err := client.Get(srv.URL + "/kieran/readerr/../../alex/readerr/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("expected a normalising redirect, got %d (handler saw %q)", res.StatusCode, saw)
	}
	if loc := res.Header.Get("Location"); loc != "/alex/readerr/" {
		t.Fatalf("Location = %q, want %q", loc, "/alex/readerr/")
	}
}
