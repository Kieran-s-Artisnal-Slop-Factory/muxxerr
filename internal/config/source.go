// Where an app's source code comes from.
//
// Three shapes are supported, and they exist because they answer different
// questions about who controls the version being built:
//
//	"../readerr"                             a sibling checkout — you are working on it
//	"apps/readerr"                           a git submodule — the version is pinned by
//	                                         this repo's commit, which is the reproducible
//	                                         option and the default here
//	"git+https://github.com/you/readerr"     cloned at build time — always the latest
//	"git+https://github.com/you/readerr#v2"  ...or a branch, tag or commit
//
// The first two are the same thing to everything downstream: a directory on
// disk. Only the third needs the build to reach the network, and only muxbuild
// ever does — the gateway reads compiled artifacts out of runtime/ and never
// looks at source at all.
//
// The submodule form is the recommended one, and the reason is worth stating:
// `git+` re-resolves on every build, so two builds a week apart can produce
// different software from identical inputs. That is convenient right up until
// you are trying to work out which build broke something. A submodule records
// an exact commit in this repo's history; `git submodule update --remote` is
// the deliberate act of moving it.
package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// RemotePrefix marks a source that must be fetched before it can be built.
const RemotePrefix = "git+"

// Remote describes a source that lives in another repository.
type Remote struct {
	// URL is the clone URL with the git+ prefix removed.
	URL string
	// Ref is a branch, tag or commit. Empty means the remote's default branch,
	// which is what you want unless you have a reason not to.
	Ref string
}

// IsRemote reports whether this app's source must be fetched.
func (a *App) IsRemote() bool { return strings.HasPrefix(a.Source, RemotePrefix) }

// Remote parses the source into a clone URL and an optional ref. The fragment
// carries the ref — "git+https://host/repo#v1.2.0" — because that is how Go
// modules, pip and npm all spell it, and inventing a fourth syntax for the
// same idea would be gratuitous.
func (a *App) Remote() (Remote, error) {
	if !a.IsRemote() {
		return Remote{}, fmt.Errorf("app %q: source %q is not a git+ URL", a.Name, a.Source)
	}
	raw := strings.TrimPrefix(a.Source, RemotePrefix)
	ref := ""
	if i := strings.LastIndex(raw, "#"); i >= 0 {
		raw, ref = raw[:i], raw[i+1:]
	}
	if raw == "" {
		return Remote{}, fmt.Errorf("app %q: source has no URL after %s", a.Name, RemotePrefix)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Remote{}, fmt.Errorf("app %q: %s is not a URL: %w", a.Name, raw, err)
	}
	switch u.Scheme {
	case "https", "http", "ssh", "git":
	case "":
		return Remote{}, fmt.Errorf("app %q: source %q needs a scheme, e.g. git+https://…", a.Name, raw)
	default:
		return Remote{}, fmt.Errorf("app %q: unsupported scheme %q", a.Name, u.Scheme)
	}
	// A URL carrying credentials would end up in the build log and in any
	// error message quoting the source. Refuse it and point at the alternative
	// rather than leaking it helpfully.
	if u.User != nil {
		return Remote{}, fmt.Errorf(
			"app %q: source URL contains credentials — use a public URL, or an SSH remote "+
				"with a key the build agent already has", a.Name)
	}
	return Remote{URL: raw, Ref: ref}, nil
}

// CacheDir is where a remote source is checked out. It sits under the runtime
// directory with the rest of the build's working state, so a `-clean` build and
// a .gitignore that already excludes runtime/ both do the right thing without
// knowing this exists.
func (c *Config) CacheDir(a *App) string {
	return filepath.Join(c.abs(c.Site.RuntimeDir), "sources", a.Name)
}
