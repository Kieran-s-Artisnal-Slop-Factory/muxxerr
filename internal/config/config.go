// Package config loads apps.json — the single declaration of which apps this
// multiplexer can serve, where their sources live, and how each one must be
// run and proxied.
//
// The file is deliberately plain JSON with no dependencies: the build
// orchestrator (cmd/muxbuild) and the gateway (cmd/mux) read the same file,
// and local-sync-template can emit an entry for a generated app without
// pulling in a YAML library.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Kind distinguishes apps that have a Go sync backend from purely static
// frontends, which the gateway serves from disk with no child process.
type Kind string

const (
	KindSync   Kind = "sync"
	KindStatic Kind = "static"
)

// DefaultPlaceholder is the sentinel base path apps are built with
// (`astro build --base=/__MUX__`). The gateway replaces it with the real
// per-user mount prefix on the way out. See internal/gateway/rewrite.go.
const DefaultPlaceholder = "/__MUX__"

// Config is the whole of apps.json.
type Config struct {
	Site Site  `json:"site"`
	Apps []App `json:"apps"`

	// Dir is the directory apps.json was loaded from; all relative paths in
	// the file resolve against it. Not serialised.
	Dir string `json:"-"`
}

// Site holds gateway-wide settings.
type Site struct {
	// Addr is the listen address, e.g. ":8080" or "127.0.0.1:8080".
	Addr string `json:"addr"`
	// DataDir holds the auth database and every per-user app database.
	DataDir string `json:"data_dir"`
	// RuntimeDir holds build output: compiled backends and built frontends.
	RuntimeDir string `json:"runtime_dir"`
	// SignupsEnabled lets anyone register. Admins can flip this at runtime;
	// the value here is only the initial default seeded on first boot.
	SignupsEnabled bool `json:"signups_enabled"`
	// SecureCookies forces the Secure flag on session cookies. Leave false
	// for plain-HTTP LAN use; set true behind TLS.
	SecureCookies bool `json:"secure_cookies"`
	// SessionTTL is how long a session lasts, e.g. "720h".
	SessionTTL Duration `json:"session_ttl"`
	// AllowAdminImpersonation lets an admin open another user's app instance.
	// Off by default: admins can already export any database, and silently
	// browsing as someone else is a bigger power than this needs.
	AllowAdminImpersonation bool `json:"allow_admin_impersonation"`
}

// App declares one multiplexable application.
type App struct {
	// Name is the URL segment: /<user>/<name>/. Lowercase, URL-safe.
	Name string `json:"name"`
	// Title and Description are shown in the app chooser.
	Title       string `json:"title"`
	Description string `json:"description"`
	// Kind is "sync" (Go backend + frontend) or "static" (frontend only).
	Kind Kind `json:"kind"`

	// Source is the app's repository root, relative to apps.json.
	Source string `json:"source"`
	// BackendDir and FrontendDir are relative to Source. BackendDir is
	// ignored for static apps.
	BackendDir  string `json:"backend_dir"`
	FrontendDir string `json:"frontend_dir"`

	// BasePlaceholder is the sentinel the frontend was built with. Empty
	// disables rewriting entirely — only correct for apps whose assets are
	// addressed purely relatively.
	BasePlaceholder string `json:"base_placeholder"`

	// HealthPath is polled after spawning to decide the instance is ready.
	HealthPath string `json:"health_path"`
	// BackupPath returns a consistent copy of the instance database; the
	// admin export endpoint proxies to it. Empty means "copy the file".
	BackupPath string `json:"backup_path"`
	// DBFile is the database filename inside the instance's data directory.
	DBFile string `json:"db_file"`

	// APIPrefixes are the request paths that are API rather than frontend.
	// They get no-store cache headers, are exempt from HTML rewriting, and
	// are what the root-absolute compatibility shim recognises. See
	// internal/gateway/shim.go.
	APIPrefixes []string `json:"api_prefixes"`

	// Env is extra environment for the child process. DB_PATH, PORT and
	// STATIC_DIR are always set by the supervisor and win over these.
	Env map[string]string `json:"env"`

	// IdleTimeout stops an instance after this long with no requests.
	// Zero means never stop.
	IdleTimeout Duration `json:"idle_timeout"`
	// AlwaysOn keeps the instance running from gateway start. Required for
	// apps that do background work — workoutt's push-notification scheduler
	// only fires while its process is alive.
	AlwaysOn bool `json:"always_on"`

	// GuardedRoutes are server-side-fetch endpoints that must not be usable
	// to reach the private network. See internal/gateway/guard.go.
	GuardedRoutes []GuardedRoute `json:"guarded_routes"`
}

// GuardedRoute blocks SSRF through an endpoint that fetches a caller-supplied
// URL — readerr's GET /title?url=... being the motivating case.
type GuardedRoute struct {
	// Path is matched exactly against the instance-relative request path.
	Path string `json:"path"`
	// Param is the query parameter holding the target URL.
	Param string `json:"param"`
	// Policy is "block-private" (reject loopback, RFC1918, link-local and
	// unique-local targets) or "allow".
	Policy string `json:"policy"`
}

// Duration is a time.Duration that unmarshals from a Go duration string.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d Duration) D() time.Duration { return time.Duration(d) }

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// Load reads and validates apps.json, filling in defaults.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	c.Dir = filepath.Dir(abs)
	if err := c.applyDefaults(); err != nil {
		return nil, err
	}
	return &c, c.Validate()
}

func (c *Config) applyDefaults() error {
	if c.Site.Addr == "" {
		c.Site.Addr = ":8080"
	}
	if c.Site.DataDir == "" {
		c.Site.DataDir = "data"
	}
	if c.Site.RuntimeDir == "" {
		c.Site.RuntimeDir = "runtime"
	}
	if c.Site.SessionTTL == 0 {
		c.Site.SessionTTL = Duration(30 * 24 * time.Hour)
	}
	for i := range c.Apps {
		a := &c.Apps[i]
		if a.Kind == "" {
			a.Kind = KindSync
		}
		if a.Title == "" {
			a.Title = a.Name
		}
		if a.FrontendDir == "" {
			a.FrontendDir = "frontend"
		}
		if a.Kind == KindSync {
			if a.BackendDir == "" {
				a.BackendDir = "backend"
			}
			if a.HealthPath == "" {
				a.HealthPath = "/healthz"
			}
			if a.DBFile == "" {
				a.DBFile = a.Name + ".db"
			}
			if a.APIPrefixes == nil {
				a.APIPrefixes = []string{"/sync/", "/healthz", "/backup"}
			}
		}
		if a.BasePlaceholder == "" {
			a.BasePlaceholder = DefaultPlaceholder
		}
	}
	return nil
}

// Validate reports every problem it finds rather than only the first, because
// a config typo should not take three restarts to diagnose.
func (c *Config) Validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	if len(c.Apps) == 0 {
		add("no apps configured")
	}
	seen := map[string]bool{}
	for _, a := range c.Apps {
		if !nameRe.MatchString(a.Name) {
			add("app %q: name must be lowercase letters, digits and dashes", a.Name)
		}
		if reservedNames[a.Name] {
			add("app %q: name is reserved by the gateway", a.Name)
		}
		if seen[a.Name] {
			add("app %q: duplicate name", a.Name)
		}
		seen[a.Name] = true
		if a.Kind != KindSync && a.Kind != KindStatic {
			add("app %q: kind must be %q or %q", a.Name, KindSync, KindStatic)
		}
		if a.Source == "" {
			add("app %q: source is required", a.Name)
		}
		if a.BasePlaceholder != "" && !strings.HasPrefix(a.BasePlaceholder, "/") {
			add("app %q: base_placeholder must start with /", a.Name)
		}
		for _, p := range a.APIPrefixes {
			if !strings.HasPrefix(p, "/") {
				add("app %q: api_prefix %q must start with /", a.Name, p)
			}
		}
		for _, g := range a.GuardedRoutes {
			if g.Policy != "block-private" && g.Policy != "allow" {
				add("app %q: guarded route %q has unknown policy %q", a.Name, g.Path, g.Policy)
			}
			if g.Param == "" {
				add("app %q: guarded route %q needs a param", a.Name, g.Path)
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid config:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// reservedNames are URL segments the gateway itself owns. They can never be an
// app name, and (in internal/store) can never be a username either — otherwise
// /login would become a user's namespace.
var reservedNames = map[string]bool{
	"login": true, "logout": true, "signup": true, "reset": true,
	"admin": true, "api": true, "assets": true, "static": true,
	"healthz": true, "favicon.ico": true, "robots.txt": true,
	"account": true, "apps": true, "_mux": true, "sync": true,
}

// Reserved reports whether a URL segment is owned by the gateway.
func Reserved(name string) bool { return reservedNames[name] }

// App looks up a configured app by name.
func (c *Config) App(name string) (*App, bool) {
	for i := range c.Apps {
		if c.Apps[i].Name == name {
			return &c.Apps[i], true
		}
	}
	return nil, false
}

// SourceDir is the app's repository root as an absolute path.
func (c *Config) SourceDir(a *App) string { return c.abs(a.Source) }

// FrontendSrc is where the frontend is built from.
func (c *Config) FrontendSrc(a *App) string {
	return filepath.Join(c.SourceDir(a), a.FrontendDir)
}

// BackendSrc is where the Go backend is built from.
func (c *Config) BackendSrc(a *App) string {
	return filepath.Join(c.SourceDir(a), a.BackendDir)
}

// DistDir is where muxbuild puts the app's built frontend.
func (c *Config) DistDir(a *App) string {
	return filepath.Join(c.abs(c.Site.RuntimeDir), "apps", a.Name, "dist")
}

// BinaryPath is where muxbuild puts the app's compiled backend.
func (c *Config) BinaryPath(a *App) string {
	name := a.Name
	if isWindows {
		name += ".exe"
	}
	return filepath.Join(c.abs(c.Site.RuntimeDir), "apps", a.Name, name)
}

// InstanceDir is the private data directory for one (user, app) pair.
func (c *Config) InstanceDir(username, app string) string {
	return filepath.Join(c.abs(c.Site.DataDir), "instances", username, app)
}

// AuthDBPath is the gateway's own database.
func (c *Config) AuthDBPath() string {
	return filepath.Join(c.abs(c.Site.DataDir), "mux.db")
}

func (c *Config) abs(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir, p)
}
