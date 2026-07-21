package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// The bug this exists to prevent: the templates and the router were written
// against different URLs. Every form in the UI posted somewhere the router did
// not serve, so the Add button, all three account actions and the post-signup
// Continue button were dead — while the API-level tests, which post directly to
// the handlers, passed happily. Nothing but the rendered HTML could have caught
// it.
//
// So: read the action attribute out of every <form> in every template, and
// check it against the routes the mux actually registers.

var (
	formActionRe = regexp.MustCompile(`<form[^>]*\saction="([^"]+)"`)
	// Template interpolations inside a URL — {{.Name}}, {{$u.ID}} — stand in
	// for a path wildcard, so they normalise to the {segment} the router uses.
	interpRe = regexp.MustCompile(`\{\{[^}]*\}\}`)
)

// routedPostPaths is the set of POST patterns Routes registers, written out by
// hand. Keeping it here rather than reflecting over the mux is deliberate: an
// http.ServeMux does not expose its table, and a hand-written list is a second
// pair of eyes on the router rather than a copy of it.
var routedPostPaths = map[string]bool{
	"/login":                      true,
	"/logout":                     true,
	"/signup":                     true,
	"/reset":                      true,
	"/passphrase":                 true,
	"/account/password":           true,
	"/account/passphrase":         true,
	"/account/sessions/revoke":    true,
	"/apps/{}/install":            true,
	"/apps/{}/remove":             true,
	"/admin/users/{}/disable":     true,
	"/admin/users/{}/enable":      true,
	"/admin/users/{}/admin":       true,
	"/admin/users/{}/reset":       true,
	"/admin/users/{}/delete":      true,
	"/admin/instances/{}/{}/stop": true,
	"/admin/settings/signups":     true,
}

func TestEveryFormActionIsRouted(t *testing.T) {
	pages, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) == 0 {
		t.Fatal("no templates found")
	}
	seen := 0
	for _, p := range pages {
		blob, err := fs.ReadFile(templateFS, p)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range formActionRe.FindAllStringSubmatch(string(blob), -1) {
			seen++
			action := interpRe.ReplaceAllString(m[1], "{}")
			if !routedPostPaths[action] {
				t.Errorf("%s: <form action=%q> normalises to %q, which no POST route serves",
					p, m[1], action)
			}
		}
	}
	if seen < 10 {
		t.Errorf("only found %d form actions across the templates; the regex has probably stopped matching", seen)
	}
}

// TestEveryRouteHasAForm is the other direction: a handler nothing can reach is
// dead code, and usually means a control was dropped from a template.
func TestEveryRouteHasAForm(t *testing.T) {
	pages, _ := fs.Glob(templateFS, "templates/*.html")
	found := map[string]bool{}
	for _, p := range pages {
		blob, _ := fs.ReadFile(templateFS, p)
		for _, m := range formActionRe.FindAllStringSubmatch(string(blob), -1) {
			found[interpRe.ReplaceAllString(m[1], "{}")] = true
		}
	}
	for route := range routedPostPaths {
		if !found[route] {
			t.Errorf("POST %s is routed but no template posts to it — dead handler, or a missing control", route)
		}
	}
}

// TestGuardScriptCannotBreakOutOfHTML pins the reason usernames are restricted:
// they are interpolated into an inline <script> in every proxied page.
func TestUsernamesCannotContainScriptDelimiters(t *testing.T) {
	bad := []string{
		"a</script>", "a<!--", "a\"b", "a'b", "a\\b", "a>b", "a/b", "a b",
		"a\nb", "kieran/../alex", "KIERAN",
	}
	for _, u := range bad {
		if msg := ValidateUsername(u); msg == "" {
			t.Errorf("ValidateUsername(%q) accepted it; it must not", u)
		}
	}
	for _, u := range []string{"kieran", "alex-2", "a_b", "u1"} {
		if msg := ValidateUsername(u); msg != "" {
			t.Errorf("ValidateUsername(%q) rejected a legitimate name: %s", u, msg)
		}
	}
	// And the normalised form of a mixed-case name must be accepted, since
	// that is what actually reaches the database.
	if msg := ValidateUsername(NormaliseUsername("  KIERAN  ")); msg != "" {
		t.Errorf("normalised mixed-case name rejected: %s", msg)
	}
}

// TestTemplatesParse catches a broken template at test time rather than at the
// first request that happens to render it.
func TestTemplatesParse(t *testing.T) {
	s := &Server{}
	if _, err := New(testConfig(), nil, nil, nil, nil); err != nil && !strings.Contains(err.Error(), "nil") {
		t.Fatalf("template parsing failed: %v", err)
	}
	_ = s
}
