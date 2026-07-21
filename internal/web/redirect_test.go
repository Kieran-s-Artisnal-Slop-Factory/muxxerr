package web

import (
	"testing"

	"local-multiplexer/internal/config"
	"local-multiplexer/internal/store"
)

func TestSafeNext(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"/kieran/readerr/", "/kieran/readerr/"},
		{"/kieran/readerr/settings/?x=1", "/kieran/readerr/settings/?x=1"},
		{"https://evil.example/", ""},
		{"//evil.example/", ""},
		{"http://evil.example", ""},
		{`/\evil.example`, ""},
		{"relative/path", ""},
		{"javascript:alert(1)", ""},
	}
	for _, c := range cases {
		if got := safeNext(c.in); got != c.want {
			t.Errorf("safeNext(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNextForUser(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	kieran := &store.User{Username: "kieran"}
	admin := &store.User{Username: "root", IsAdmin: true}

	cases := []struct {
		name string
		next string
		user *store.User
		want string
	}{
		{"own app deep link", "/kieran/readerr/", kieran, "/kieran/readerr/"},
		{"own app deep page", "/kieran/readerr/settings/", kieran, "/kieran/readerr/settings/"},
		{"someone else's app", "/alex/readerr/", kieran, "/"},
		{"reserved path is fine", "/account", kieran, "/account"},
		{"empty", "", kieran, "/"},
		{"external", "https://evil.example/", kieran, "/"},
		{"admin without impersonation", "/kieran/readerr/", admin, "/"},
	}
	for _, c := range cases {
		if got := s.nextForUser(c.next, c.user); got != c.want {
			t.Errorf("%s: nextForUser(%q, %s) = %q, want %q", c.name, c.next, c.user.Username, got, c.want)
		}
	}

	s.cfg.Site.AllowAdminImpersonation = true
	if got := s.nextForUser("/kieran/readerr/", admin); got != "/kieran/readerr/" {
		t.Errorf("admin with impersonation on: got %q, want the deep link", got)
	}
}

// testConfig is the minimal config the template-parsing test needs.
func testConfig() *config.Config {
	return &config.Config{Site: config.Site{Addr: ":0"}}
}
